package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
)

type stubWebhooks struct {
	outcome   webhookapp.Outcome
	err       error
	received  webhookapp.ReceiveInput
	callCount int
}

func (s *stubWebhooks) Receive(_ context.Context, in webhookapp.ReceiveInput) (webhookapp.Outcome, error) {
	s.callCount++
	s.received = in
	if in.ReadError != nil {
		return "", in.ReadError
	}
	return s.outcome, s.err
}

func postWebhook(t *testing.T, handler http.Handler, body []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/v1/webhooks/stripe", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if signature != "" {
		request.Header.Set("Stripe-Signature", signature)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// The endpoint is reachable without an API key: the provider has no way to
// hold one, and trust comes from the signature instead.
func TestWebhookDoesNotRequireAPIKey(t *testing.T) {
	t.Parallel()

	webhooks := &stubWebhooks{outcome: webhookapp.OutcomeAccepted}
	handler := newTestHandlerWith(nil, nil, webhooks)

	response := postWebhook(t, handler, []byte(`{"id":"evt_1"}`), "t=1,v1=abc")
	if response.Code == http.StatusUnauthorized {
		t.Fatal("the webhook must not be behind the integration API key")
	}
	if webhooks.callCount != 1 {
		t.Fatalf("callCount = %d, want the use case to run", webhooks.callCount)
	}
}

func TestWebhookAcceptedReturns202(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWith(nil, nil, &stubWebhooks{outcome: webhookapp.OutcomeAccepted})

	response := postWebhook(t, handler, []byte(`{"id":"evt_1"}`), "t=1,v1=abc")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a newly stored event", response.Code)
	}
}

// Duplicate and ignored are both no-ops, and both must stop the provider from
// retrying, so they share the 200.
func TestWebhookNoOpOutcomesReturn200(t *testing.T) {
	t.Parallel()

	for _, outcome := range []webhookapp.Outcome{webhookapp.OutcomeDuplicate, webhookapp.OutcomeIgnored} {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()
			handler := newTestHandlerWith(nil, nil, &stubWebhooks{outcome: outcome})

			response := postWebhook(t, handler, []byte(`{"id":"evt_1"}`), "t=1,v1=abc")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for %s", response.Code, outcome)
			}
		})
	}
}

func TestWebhookInvalidSignatureReturns400(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWith(nil, nil, &stubWebhooks{
		err: webhookapp.ErrInvalidSignature,
	})

	response := postWebhook(t, handler, []byte(`{"id":"evt_1"}`), "t=1,v1=forged")
	// 400 says the request itself is the problem. Stripe still redelivers on
	// any non-2xx, so this does not stop the retries; what it does is keep an
	// unverifiable request from being recorded as accepted, and mark it in our
	// logs as something no redelivery can fix.
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unverifiable signature", response.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["code"] != codeInvalidSignature {
		t.Fatalf("code = %v, want %s", body["code"], codeInvalidSignature)
	}
}

// A missing header takes the same path as a forged one, so the response never
// tells an attacker which of the two it was.
func TestWebhookMissingSignatureReturns400(t *testing.T) {
	t.Parallel()

	webhooks := &stubWebhooks{err: webhookapp.ErrInvalidSignature}
	handler := newTestHandlerWith(nil, nil, webhooks)

	response := postWebhook(t, handler, []byte(`{"id":"evt_1"}`), "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when the signature header is absent", response.Code)
	}
	if webhooks.received.Signature != "" {
		t.Fatal("an absent header must reach the use case as an empty signature")
	}
}

// Storage failure is the case where a retry is exactly what we want, so the
// provider must be told to come back.
func TestWebhookStorageFailureReturns500(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWith(nil, nil, &stubWebhooks{err: errors.New("postgres down")})

	response := postWebhook(t, handler, []byte(`{"id":"evt_1"}`), "t=1,v1=abc")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so the provider redelivers", response.Code)
	}
}

// An oversized body cannot be verified, but the cause is our own limit, so it
// must not be answered like a forged signature: 500 keeps the event alive.
func TestWebhookOversizedBodyReturns500(t *testing.T) {
	t.Parallel()

	webhooks := &stubWebhooks{outcome: webhookapp.OutcomeAccepted}
	handler := newTestHandlerWith(nil, nil, webhooks)

	oversized := []byte(`{"padding":"` + strings.Repeat("a", webhookMaxBodyBytes+1) + `"}`)
	response := postWebhook(t, handler, oversized, "t=1,v1=abc")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so a corrected limit can still receive it", response.Code)
	}
	if webhooks.callCount != 1 {
		t.Fatal("an unreadable body must reach the use case once for observability")
	}
	if !errors.Is(webhooks.received.ReadError, webhookapp.ErrPayloadTooLarge) {
		t.Fatalf("ReadError = %v, want ErrPayloadTooLarge", webhooks.received.ReadError)
	}
	if webhooks.received.RawBody != nil {
		t.Fatal("a partial oversized body must not cross the transport boundary")
	}
}

// The webhook gets its own headroom: a body that the integrator routes would
// reject must still be accepted here.
func TestWebhookAcceptsBodyLargerThanTheIntegratorLimit(t *testing.T) {
	t.Parallel()

	webhooks := &stubWebhooks{outcome: webhookapp.OutcomeAccepted}
	handler := newTestHandlerWith(nil, nil, webhooks)

	body := []byte(`{"padding":"` + strings.Repeat("a", maxBodyBytes*2) + `"}`)
	response := postWebhook(t, handler, body, "t=1,v1=abc")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a large but acceptable event", response.Code)
	}
	if len(webhooks.received.RawBody) != len(body) {
		t.Fatalf("RawBody length = %d, want the whole body %d", len(webhooks.received.RawBody), len(body))
	}
}

// The bytes handed to verification must be the bytes that arrived: parsing and
// re-encoding would change key order and whitespace, and no signature computed
// over the original would ever match again.
func TestWebhookForwardsRawBytesUntouched(t *testing.T) {
	t.Parallel()

	webhooks := &stubWebhooks{outcome: webhookapp.OutcomeAccepted}
	handler := newTestHandlerWith(nil, nil, webhooks)

	raw := []byte("{\n  \"id\" : \"evt_1\",\n  \"type\":\"checkout.session.completed\"\n}\n")
	postWebhook(t, handler, raw, "t=1,v1=abc")

	if string(webhooks.received.RawBody) != string(raw) {
		t.Fatalf("RawBody = %q, want the exact request bytes %q", webhooks.received.RawBody, raw)
	}
}

func TestWebhookPassesSignatureAndCorrelation(t *testing.T) {
	t.Parallel()

	webhooks := &stubWebhooks{outcome: webhookapp.OutcomeAccepted}
	handler := newTestHandlerWith(nil, nil, webhooks)

	postWebhook(t, handler, []byte(`{"id":"evt_1"}`), "t=1,v1=abc")
	if webhooks.received.Signature != "t=1,v1=abc" {
		t.Fatalf("Signature = %q, want the header value", webhooks.received.Signature)
	}
	if webhooks.received.CorrelationID == "" {
		t.Fatal("the use case must receive the request correlation id")
	}
}

// The worker shares the contract but serves only health.
func TestWebhookNotServedWithoutReceiver(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWith(nil, nil, nil)

	response := postWebhook(t, handler, []byte(`{"id":"evt_1"}`), "t=1,v1=abc")
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 when the process does not serve webhooks", response.Code)
	}
}
