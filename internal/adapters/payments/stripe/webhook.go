package stripe

import (
	"encoding/json"
	"fmt"
	"time"

	app "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	stripesdk "github.com/stripe/stripe-go/v86"
	stripewebhook "github.com/stripe/stripe-go/v86/webhook"
)

// eventKinds translates Stripe event names into the application's vocabulary.
// An event missing from this table is recorded and ignored, so adding a new
// Stripe event never happens by accident.
var eventKinds = map[string]domain.Kind{
	"checkout.session.completed":               domain.KindCheckoutCompleted,
	"checkout.session.async_payment_succeeded": domain.KindCheckoutPaymentSucceeded,
	"checkout.session.async_payment_failed":    domain.KindCheckoutPaymentFailed,
	"checkout.session.expired":                 domain.KindCheckoutExpired,
}

// Webhook verifies Stripe signatures over the exact bytes received.
type Webhook struct {
	secret    string
	tolerance time.Duration
}

// NewWebhook creates the verifier for one endpoint signing secret.
func NewWebhook(secret string) *Webhook {
	return &Webhook{secret: secret, tolerance: stripewebhook.DefaultTolerance}
}

// Provider identifies which provider signed the events this adapter accepts.
func (w *Webhook) Provider() domain.Provider { return domain.Stripe }

// Verify checks the Stripe-Signature header against the raw request body.
//
// The signature covers the bytes exactly as they arrived, so they must never be
// deserialized and re-encoded before this runs: key order and whitespace would
// change and the computed signature would not match.
//
// API version mismatches are deliberately ignored here. This endpoint only
// stores the event; nothing interprets the provider's object graph yet, and a
// version difference must not stop an authentic event from being recorded. The
// consumer checks compatibility when it actually reads the session.
func (w *Webhook) Verify(rawBody []byte, signature string) (app.ProviderEvent, error) {
	event, err := stripewebhook.ConstructEventWithOptions(rawBody, signature, w.secret,
		stripewebhook.ConstructEventOptions{
			Tolerance:                w.tolerance,
			IgnoreAPIVersionMismatch: true,
		})
	if err != nil {
		return app.ProviderEvent{}, fmt.Errorf("verify stripe signature: %w", err)
	}
	return app.ProviderEvent{
		ID:         event.ID,
		Type:       string(event.Type),
		Kind:       eventKinds[string(event.Type)],
		OccurredAt: time.Unix(event.Created, 0).UTC(),
		Payload:    canonicalPayload(rawBody, event),
	}, nil
}

// canonicalPayload returns the bytes stored in the queryable payload column.
// The raw bytes are kept verbatim elsewhere; this copy only has to be valid
// JSON, so a provider that ever signs something else still gets recorded.
func canonicalPayload(rawBody []byte, event stripesdk.Event) json.RawMessage {
	if json.Valid(rawBody) {
		return json.RawMessage(rawBody)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}
