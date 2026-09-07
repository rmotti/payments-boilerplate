package stripe

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	app "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	payments "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	stripesdk "github.com/stripe/stripe-go/v86"
)

// sessionEvent builds a stored inbox entry the way the receiving endpoint
// writes one: the provider's raw body, parsed into the payload column.
func sessionEvent(t *testing.T, kind domain.Kind, eventType, session string) domain.Event {
	t.Helper()

	raw := []byte(session)
	event, err := domain.NewEvent("evt_local_1", domain.Stripe, "evt_provider_1", eventType,
		kind, raw, json.RawMessage(raw), time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build stored event: %v", err)
	}
	return event
}

// sessionPayload mirrors the envelope Stripe delivers, with the metadata this
// application attaches when it opens a session.
func sessionPayload(apiVersion, eventType, paymentStatus string, amount int64) string {
	return fmt.Sprintf(`{
      "id":"evt_provider_1","object":"event","api_version":%q,"type":%q,
      "data":{"object":{
        "id":"cs_test_1","object":"checkout.session",
        "payment_status":%q,"status":"complete",
        "amount_total":%d,"currency":"brl",
        "client_reference_id":"ord_1",
        "payment_intent":{"id":"pi_1","object":"payment_intent"},
        "metadata":{"order_id":"ord_1","payment_id":"pay_1","payment_attempt_id":"pat_1"}
      }}}`, apiVersion, eventType, paymentStatus, amount)
}

func TestInterpretReadsTheStoredSession(t *testing.T) {
	t.Parallel()

	event := sessionEvent(t, domain.KindCheckoutCompleted, "checkout.session.completed",
		sessionPayload(stripesdk.APIVersion, "checkout.session.completed", "paid", 10000))

	got, err := NewSession().Interpret(event)
	if err != nil {
		t.Fatalf("Interpret() error = %v", err)
	}
	want := app.SessionOutcome{
		OrderID: "ord_1", PaymentID: "pay_1", AttemptID: "pat_1",
		SessionID: "cs_test_1", PaymentIntentID: "pi_1",
		Amount: 10000, Currency: "BRL",
		Outcome:    payments.OutcomeSucceeded,
		OccurredAt: event.ReceivedAt.UTC(),
	}
	if got != want {
		t.Errorf("Interpret()\n got = %#v\nwant = %#v", got, want)
	}
}

// The case that separates a card integration from a correct one. A completed
// session with an unpaid status is the normal Pix and boleto path, and must
// produce processing rather than a paid order.
func TestCompletedSessionWithoutPaymentIsPendingNotPaid(t *testing.T) {
	t.Parallel()

	event := sessionEvent(t, domain.KindCheckoutCompleted, "checkout.session.completed",
		sessionPayload(stripesdk.APIVersion, "checkout.session.completed", "unpaid", 10000))

	got, err := NewSession().Interpret(event)
	if err != nil {
		t.Fatalf("Interpret() error = %v", err)
	}
	if got.Outcome != payments.OutcomePending {
		t.Errorf("Outcome = %q, want %q for an unpaid completed session",
			got.Outcome, payments.OutcomePending)
	}
}

func TestInterpretMapsEachHandledKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind      domain.Kind
		eventType string
		want      payments.Outcome
	}{
		{domain.KindCheckoutPaymentSucceeded, "checkout.session.async_payment_succeeded", payments.OutcomeSucceeded},
		{domain.KindCheckoutPaymentFailed, "checkout.session.async_payment_failed", payments.OutcomeFailed},
		{domain.KindCheckoutExpired, "checkout.session.expired", payments.OutcomeExpired},
	}

	for _, test := range tests {
		t.Run(test.eventType, func(t *testing.T) {
			t.Parallel()

			// payment_status is deliberately unpaid here: for these kinds the
			// event type decides the outcome, not the session's own status.
			event := sessionEvent(t, test.kind, test.eventType,
				sessionPayload(stripesdk.APIVersion, test.eventType, "unpaid", 10000))

			got, err := NewSession().Interpret(event)
			if err != nil {
				t.Fatalf("Interpret() error = %v", err)
			}
			if got.Outcome != test.want {
				t.Errorf("Outcome = %q, want %q", got.Outcome, test.want)
			}
		})
	}
}

func TestInterpretRejectsPayloadsItCannotTrust(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    domain.Kind
		payload string
		want    error
	}{
		{
			name:    "unknown api version",
			kind:    domain.KindCheckoutCompleted,
			payload: sessionPayload("2019-01-01", "checkout.session.completed", "paid", 10000),
			want:    app.ErrIncompatibleAPIVersion,
		},
		{
			name: "absent api version is never assumed compatible",
			kind: domain.KindCheckoutCompleted,
			payload: `{"id":"evt_provider_1","object":"event","type":"checkout.session.completed",
			  "data":{"object":{"id":"cs_test_1","object":"checkout.session","payment_status":"paid",
			  "metadata":{"order_id":"ord_1","payment_id":"pay_1","payment_attempt_id":"pat_1"}}}}`,
			want: app.ErrIncompatibleAPIVersion,
		},
		{
			name: "missing attempt metadata",
			kind: domain.KindCheckoutCompleted,
			payload: fmt.Sprintf(`{"id":"evt_provider_1","object":"event","api_version":%q,
			  "type":"checkout.session.completed","data":{"object":{"id":"cs_test_1",
			  "object":"checkout.session","payment_status":"paid","amount_total":10000,
			  "currency":"brl","metadata":{"order_id":"ord_1","payment_id":"pay_1"}}}}`,
				stripesdk.APIVersion),
			want: app.ErrMissingReference,
		},
		{
			name:    "unknown payment status on a completed session",
			kind:    domain.KindCheckoutCompleted,
			payload: sessionPayload(stripesdk.APIVersion, "checkout.session.completed", "something_new", 10000),
			want:    app.ErrUnreadablePayload,
		},
		{
			name: "event carries an object that is not a session",
			kind: domain.KindCheckoutCompleted,
			payload: fmt.Sprintf(`{"id":"evt_provider_1","object":"event","api_version":%q,
			  "type":"checkout.session.completed","data":{"object":{"id":"pi_1",
			  "object":"payment_intent"}}}`, stripesdk.APIVersion),
			want: app.ErrUnreadablePayload,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			event := sessionEvent(t, test.kind, "checkout.session.completed", test.payload)
			_, err := NewSession().Interpret(event)
			if !errors.Is(err, test.want) {
				t.Errorf("Interpret() error = %v, want %v", err, test.want)
			}
		})
	}
}

// A message whose stored event maps to no handled kind means the message and
// the inbox disagree: the endpoint stores unhandled types as skipped and never
// produces a message for them. It must stop rather than be guessed at.
func TestInterpretRefusesAnUnhandledKind(t *testing.T) {
	t.Parallel()

	event := sessionEvent(t, domain.KindUnknown, "checkout.session.completed",
		sessionPayload(stripesdk.APIVersion, "customer.created", "paid", 10000))
	event.Type = "customer.created"

	if _, err := NewSession().Interpret(event); !errors.Is(err, app.ErrUnreadablePayload) {
		t.Errorf("Interpret() error = %v, want %v", err, app.ErrUnreadablePayload)
	}
}

// The kind travels in the provider's own vocabulary in the inbox column, so
// the adapter must be able to interpret an entry whose neutral kind was never
// resolved by the caller.
func TestInterpretDerivesTheKindFromTheStoredEventType(t *testing.T) {
	t.Parallel()

	event := sessionEvent(t, domain.KindUnknown, "checkout.session.expired",
		sessionPayload(stripesdk.APIVersion, "checkout.session.expired", "unpaid", 10000))
	event.Type = "checkout.session.expired"

	got, err := NewSession().Interpret(event)
	if err != nil {
		t.Fatalf("Interpret() error = %v", err)
	}
	if got.Outcome != payments.OutcomeExpired {
		t.Errorf("Outcome = %q, want %q", got.Outcome, payments.OutcomeExpired)
	}
}

// The version constant must track the pinned SDK, or the adapter would claim
// to understand a shape its own types no longer match.
func TestSupportedVersionTracksThePinnedSDK(t *testing.T) {
	t.Parallel()

	if SupportedAPIVersion != stripesdk.APIVersion {
		t.Errorf("SupportedAPIVersion = %q, want the pinned SDK version %q",
			SupportedAPIVersion, stripesdk.APIVersion)
	}
}
