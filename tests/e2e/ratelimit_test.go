//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/platform/config"
)

// TestRateLimitRefusesOverTheWire proves the limiter against the real process
// over a real socket, which is the one thing the handler tests cannot do: the
// limits are enforced by the composition the binary runs, and the 429 travels
// as an actual HTTP response with its headers intact.
//
// The limit is lowered for this scenario alone, because reaching the shipped
// default over the network would mean sending 1200 requests to prove something
// the unit tests already prove exactly.
func TestRateLimitRefusesOverTheWire(t *testing.T) {
	const burst = 5
	previous := runtimeOverrides
	runtimeOverrides = func(cfg *config.Config) {
		cfg.RateLimitClientBurst = burst
		cfg.RateLimitClientInterval = time.Minute
		// Leave the credential limit wide so the coarse one is unambiguously
		// what refuses: the assertion below is about the outermost gate.
		cfg.RateLimitCredentialBurst = 10000
		cfg.RateLimitCredentialInterval = time.Minute
	}
	t.Cleanup(func() { runtimeOverrides = previous })

	h := newHarness(t, false)

	// The probe has its own bucket, so spending the coarse one must not make
	// the deployment look unhealthy. Read it before and after.
	if response, _ := h.rawRequest(h.apiBaseURL, http.MethodGet, "/health", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("health before the limit was reached = %d, want 200", response.StatusCode)
	}

	var refused capturedResponse
	var refusedBody []byte
	for attempt := 1; attempt <= burst+1; attempt++ {
		response, body := h.rawRequest(h.apiBaseURL, http.MethodGet, "/v1/orders/ord_"+
			"0123456789abcdef0123456789abcdef", apiKeyA)
		if response.StatusCode == http.StatusTooManyRequests {
			if attempt <= burst {
				t.Fatalf("request %d was refused inside a burst of %d", attempt, burst)
			}
			refused, refusedBody = response, body
			break
		}
	}
	if refused.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("no request was refused after the burst of %d was spent", burst)
	}

	// A 429 is a response like any other: the security headers and the
	// correlation id are present, and no CORS header appears.
	assertSecurityHeaders(t, refused, "rate limited request")

	retryAfter := refused.Header.Get("Retry-After")
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After = %q, want whole seconds: %v", retryAfter, err)
	}
	if seconds < 1 || seconds > 60 {
		t.Fatalf("Retry-After = %d, want between 1 and the 60s refill window", seconds)
	}

	var body struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		CorrelationID string `json:"correlationId"`
	}
	if err := json.Unmarshal(refusedBody, &body); err != nil {
		t.Fatalf("decode rate limited body: %v (%s)", err, refusedBody)
	}
	if body.Code != "rate_limited" {
		t.Fatalf("code = %q, want rate_limited", body.Code)
	}
	if body.CorrelationID != refused.Header.Get("X-Correlation-ID") {
		t.Fatal("the body correlation id does not match the response header")
	}
	// The refusal must not name the credential it was counting, nor confirm
	// which limit was reached.
	if string(refusedBody) != "" && containsAny(string(refusedBody), apiKeyA, apiKeyB, "client", "credential") {
		t.Fatalf("the rate limited body leaks the identity that was limited: %s", refusedBody)
	}

	// The probe still answers: its bucket was never touched by the traffic
	// above, which is the whole point of giving health its own.
	if response, _ := h.rawRequest(h.apiBaseURL, http.MethodGet, "/health", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("health after the business limit was reached = %d, want 200", response.StatusCode)
	}

	// Nothing about the limiting reached the logs in identifying form.
	if logs := h.logs.String(); containsAny(logs, apiKeyA, apiKeyB) {
		t.Fatal("a credential appears in the process logs")
	}
}

// containsAny reports whether any needle appears in haystack, skipping empty
// ones so an unset credential cannot match everything.
func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
