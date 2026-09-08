//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

var documentationPaths = []string{"/docs", "/docs/", "/openapi.yaml"}

// assertSecurityHeaders checks the headers every response of every process
// must carry, whatever its status.
func assertSecurityHeaders(t *testing.T, response capturedResponse, path string) {
	t.Helper()
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
		"X-Frame-Options":        "DENY",
	}
	for name, value := range want {
		if got := response.Header.Get(name); got != value {
			t.Fatalf("%s %s = %q, want %q", path, name, got, value)
		}
	}
	if response.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("%s has no Content-Security-Policy", path)
	}
	if response.Header.Get("X-Correlation-ID") == "" {
		t.Fatalf("%s has no X-Correlation-ID", path)
	}
	for name := range response.Header {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Fatalf("%s emits CORS header %s without configuration", path, name)
		}
	}
}

// TestDocumentationIsAbsentWithoutOptIn is the deployed default: no
// DOCS_ENABLED, no documentation, in an environment that is not development.
func TestDocumentationIsAbsentWithoutOptIn(t *testing.T) {
	h := newConfiguredHarness(t, false, "production", false)

	unknown, _ := h.rawRequest(h.apiBaseURL, http.MethodGet, "/definitely-not-registered", "")
	for _, path := range documentationPaths {
		for _, key := range []string{"", apiKeyA} {
			response, body := h.rawRequest(h.apiBaseURL, http.MethodGet, path, key)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("GET %s (key %q) status = %d, want 404; body=%s", path, key, response.StatusCode, body)
			}
			if response.Header.Get("Content-Type") != unknown.Header.Get("Content-Type") {
				t.Fatalf("GET %s is distinguishable from an unknown path", path)
			}
			assertSecurityHeaders(t, response, path)
		}
	}
}

// TestDocumentationOutsideDevelopmentRequiresAValidKey covers the opt-in in a
// deployed environment: the routes exist, and only a valid key reaches them.
func TestDocumentationOutsideDevelopmentRequiresAValidKey(t *testing.T) {
	h := newConfiguredHarness(t, false, "production", true)

	for _, path := range documentationPaths {
		for _, key := range []string{"", "not-a-configured-key"} {
			response, body := h.rawRequest(h.apiBaseURL, http.MethodGet, path, key)
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("GET %s (key %q) status = %d, want 401; body=%s", path, key, response.StatusCode, body)
			}
			if !strings.Contains(string(body), `"unauthorized"`) {
				t.Fatalf("GET %s (key %q) body = %s, want the unauthorized envelope", path, key, body)
			}
			assertSecurityHeaders(t, response, path)
		}
	}

	wantStatus := map[string]int{"/docs": http.StatusMovedPermanently, "/docs/": http.StatusOK, "/openapi.yaml": http.StatusOK}
	for _, path := range documentationPaths {
		response, body := h.rawRequest(h.apiBaseURL, http.MethodGet, path, apiKeyA)
		if response.StatusCode != wantStatus[path] {
			t.Fatalf("GET %s (valid key) status = %d, want %d; body=%s", path, response.StatusCode, wantStatus[path], body)
		}
		assertSecurityHeaders(t, response, path)
	}

	// The warning names the environment and never the credential.
	logs := h.logs.String()
	if !strings.Contains(logs, "documentation enabled outside development") {
		t.Fatalf("startup did not warn about documentation outside development\nlogs:\n%s", logs)
	}
	for _, secret := range []string{apiKeyA, apiKeyB, webhookSecret} {
		if strings.Contains(logs, secret) {
			t.Fatal("startup logs contain a credential")
		}
	}
}

// TestDocumentationInDevelopmentIsServedWhenEnabled is the local experience
// the Compose file and .env.example configure.
func TestDocumentationInDevelopmentIsServedWhenEnabled(t *testing.T) {
	h := newConfiguredHarness(t, false, "development", true)

	index, body := h.rawRequest(h.apiBaseURL, http.MethodGet, "/docs/", "")
	if index.StatusCode != http.StatusOK || !strings.Contains(string(body), "swagger-ui") {
		t.Fatalf("GET /docs/ status = %d, want 200 with Swagger UI", index.StatusCode)
	}
	policy := index.Header.Get("Content-Security-Policy")
	if !strings.Contains(policy, "'sha256-") || strings.Contains(policy, "unsafe-inline'; script") {
		t.Fatalf("Swagger policy = %q, want hashed inline scripts", policy)
	}
	spec, specBody := h.rawRequest(h.apiBaseURL, http.MethodGet, "/openapi.yaml", "")
	if spec.StatusCode != http.StatusOK || spec.Header.Get("Content-Type") != "application/yaml" {
		t.Fatalf("GET /openapi.yaml status/type = %d/%q", spec.StatusCode, spec.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(specBody), "openapi:") {
		t.Fatalf("GET /openapi.yaml body is not the contract: %.80s", specBody)
	}
	redirect, _ := h.rawRequest(h.apiBaseURL, http.MethodGet, "/docs", "")
	if redirect.StatusCode != http.StatusMovedPermanently || redirect.Header.Get("Location") != "/docs/" {
		t.Fatalf("GET /docs status/location = %d/%q, want 301 to /docs/", redirect.StatusCode, redirect.Header.Get("Location"))
	}
	for _, path := range documentationPaths {
		response, _ := h.rawRequest(h.apiBaseURL, http.MethodGet, path, "")
		assertSecurityHeaders(t, response, path)
	}
}

// TestWorkerServesOnlyHealth exercises the real worker process. Every route
// other than health must be absent, not present and refused.
func TestWorkerServesOnlyHealth(t *testing.T) {
	h := newConfiguredHarness(t, true, "production", true)

	health, body := h.rawRequest(h.workerBaseURL, http.MethodGet, "/health", "")
	if health.StatusCode != http.StatusOK {
		t.Fatalf("worker GET /health status = %d, want 200; body=%s", health.StatusCode, body)
	}
	assertSecurityHeaders(t, health, "/health")

	absent := append([]string{
		"/v1/orders", "/v1/orders/ord_1", "/v1/orders/ord_1/checkout",
		"/v1/webhook-events", "/v1/webhooks/stripe",
	}, documentationPaths...)
	for _, path := range absent {
		for _, key := range []string{"", apiKeyA} {
			response, responseBody := h.rawRequest(h.workerBaseURL, http.MethodGet, path, key)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("worker GET %s (key %q) status = %d, want 404; body=%s",
					path, key, response.StatusCode, responseBody)
			}
			assertSecurityHeaders(t, response, path)
		}
	}
}

// TestAPIResponsesCarrySecurityHeaders checks the contract routes, which the
// documentation tests do not touch, across success and error statuses.
func TestAPIResponsesCarrySecurityHeaders(t *testing.T) {
	h := newHarness(t, false)

	healthResponse, _ := h.request(http.MethodGet, "/health", "", nil, nil)
	assertSecurityHeaders(t, healthResponse, "/health")

	unauthorized, _ := h.request(http.MethodPost, "/v1/orders", "", map[string]any{
		"productId": "product_demo", "quantity": 1,
	}, map[string]string{"Idempotency-Key": "headers-401"})
	requireStatus(t, unauthorized, nil, http.StatusUnauthorized)
	assertSecurityHeaders(t, unauthorized, "/v1/orders")

	created, body := h.request(http.MethodPost, "/v1/orders", apiKeyA, map[string]any{
		"productId": "product_demo", "quantity": 1,
	}, map[string]string{"Idempotency-Key": "headers-201"})
	requireStatus(t, created, body, http.StatusCreated)
	assertSecurityHeaders(t, created, "/v1/orders")

	notFound, notFoundBody := h.request(http.MethodGet, "/v1/orders/ord_0123456789abcdef0123456789abcdef", apiKeyA, nil, nil)
	requireStatus(t, notFound, notFoundBody, http.StatusNotFound)
	assertSecurityHeaders(t, notFound, "/v1/orders/{orderId}")

	unknown, _ := h.rawRequest(h.apiBaseURL, http.MethodGet, "/not-a-route", "")
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /not-a-route status = %d, want 404", unknown.StatusCode)
	}
	assertSecurityHeaders(t, unknown, "/not-a-route")
}
