package stripe

import (
	"encoding/json"
	"fmt"
	"strings"

	app "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	payments "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	stripesdk "github.com/stripe/stripe-go/v86"
)

// SupportedAPIVersion is the Stripe API version whose Checkout Session shape
// this adapter is written against.
//
// It is taken from the pinned SDK rather than written out, so bumping the
// dependency cannot leave a stale literal here claiming to support a shape the
// types no longer match.
//
// The receiving endpoint deliberately ignores version mismatches, because it
// only stores bytes. Here the object graph is actually read, so a version the
// adapter does not understand must stop the event rather than be guessed at.
// See ADR 0011.
const SupportedAPIVersion = stripesdk.APIVersion

// metadata keys this application attaches when opening a session. They are the
// link from a provider event back to local rows.
const (
	metadataOrderID   = "order_id"
	metadataPaymentID = "payment_id"
	metadataAttemptID = "payment_attempt_id"
)

// eventEnvelope is the part of the stored event this adapter reads. The SDK's
// own Event type is not used because the stored payload is the raw provider
// body, and only these fields are needed to reach the session.
type eventEnvelope struct {
	APIVersion string `json:"api_version"`
	Type       string `json:"type"`
	Data       struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// Session interprets stored Checkout Session events.
type Session struct {
	// supportedVersions lists the API versions whose payload shape this
	// adapter can read. It is a set rather than a single value so a version
	// bump can be rolled out without a flag day.
	supportedVersions map[string]struct{}
}

// NewSession creates the interpreter for stored Stripe events.
func NewSession() *Session {
	return &Session{
		supportedVersions: map[string]struct{}{SupportedAPIVersion: {}},
	}
}

// Provider identifies which provider's payloads this adapter reads.
func (s *Session) Provider() domain.Provider { return domain.Stripe }

// Interpret reads the stored payload and reports what it means locally.
//
// It reads the persisted copy rather than calling the provider back. That
// keeps the consumer working while the provider is unreachable, and makes
// reprocessing deterministic: the same stored bytes always produce the same
// decision.
func (s *Session) Interpret(event domain.Event) (app.SessionOutcome, error) {
	var envelope eventEnvelope
	if err := json.Unmarshal(event.Payload, &envelope); err != nil {
		return app.SessionOutcome{}, fmt.Errorf("%w: decode event envelope: %w",
			app.ErrUnreadablePayload, err)
	}

	// An unknown version is checked before anything is read out of the graph,
	// so a renamed or re-typed field can never be silently misread.
	if err := s.checkVersion(envelope.APIVersion); err != nil {
		return app.SessionOutcome{}, err
	}

	var session stripesdk.CheckoutSession
	if err := json.Unmarshal(envelope.Data.Object, &session); err != nil {
		return app.SessionOutcome{}, fmt.Errorf("%w: decode checkout session: %w",
			app.ErrUnreadablePayload, err)
	}
	if session.Object != "" && session.Object != "checkout.session" {
		return app.SessionOutcome{}, fmt.Errorf("%w: event carries a %q, not a checkout session",
			app.ErrUnreadablePayload, session.Object)
	}
	if strings.TrimSpace(session.ID) == "" {
		return app.SessionOutcome{}, fmt.Errorf("%w: checkout session has no id",
			app.ErrUnreadablePayload)
	}

	// The kind is derived from the provider's stored event name rather than
	// read off the entry: the inbox column holds the provider's vocabulary,
	// and translating it is this adapter's job. Falling back to the entry's
	// own kind keeps a caller that already resolved it working.
	kind := event.Kind
	if kind == domain.KindUnknown {
		kind = eventKinds[event.Type]
	}

	outcome, err := financialOutcome(kind, session)
	if err != nil {
		return app.SessionOutcome{}, err
	}

	reference, err := localReference(session)
	if err != nil {
		return app.SessionOutcome{}, err
	}

	paymentIntentID := ""
	if session.PaymentIntent != nil {
		paymentIntentID = session.PaymentIntent.ID
	}

	return app.SessionOutcome{
		OrderID:         reference.orderID,
		PaymentID:       reference.paymentID,
		AttemptID:       reference.attemptID,
		SessionID:       session.ID,
		PaymentIntentID: paymentIntentID,
		Amount:          session.AmountTotal,
		Currency:        strings.ToUpper(string(session.Currency)),
		Outcome:         outcome,
		OccurredAt:      event.ReceivedAt.UTC(),
	}, nil
}

func (s *Session) checkVersion(version string) error {
	// An event with no version recorded is refused rather than assumed
	// compatible: guessing is exactly the failure this check exists to stop.
	if strings.TrimSpace(version) == "" {
		return fmt.Errorf("%w: event carries no api_version", app.ErrIncompatibleAPIVersion)
	}
	if _, ok := s.supportedVersions[version]; !ok {
		return fmt.Errorf("%w: %s is not among the supported versions",
			app.ErrIncompatibleAPIVersion, version)
	}
	return nil
}

type reference struct {
	orderID   string
	paymentID string
	attemptID string
}

// localReference pulls the identifiers this application attached when it
// opened the session.
//
// The attempt is what the transition acts on, so its absence is fatal. The
// order also travels in client_reference_id, which is used as a fallback: the
// two are written together, and a session missing one but not the other is
// worth surviving.
func localReference(session stripesdk.CheckoutSession) (reference, error) {
	found := reference{
		orderID:   strings.TrimSpace(session.Metadata[metadataOrderID]),
		paymentID: strings.TrimSpace(session.Metadata[metadataPaymentID]),
		attemptID: strings.TrimSpace(session.Metadata[metadataAttemptID]),
	}
	if found.orderID == "" {
		found.orderID = strings.TrimSpace(session.ClientReferenceID)
	}
	if found.attemptID == "" || found.paymentID == "" || found.orderID == "" {
		return reference{}, fmt.Errorf(
			"%w: session %s is missing order, payment or attempt metadata",
			app.ErrMissingReference, session.ID)
	}
	return found, nil
}

// financialOutcome maps the event kind and the session state onto the
// vocabulary of the transition matrix.
//
// A completed session is the case that matters: it does not mean paid. With
// Pix, boleto and every late-confirmation method the session completes before
// the money arrives, and only the later asynchronous event settles it.
// Treating completion as payment would work with cards and would mark an order
// paid without a payment on the first Pix.
func financialOutcome(kind domain.Kind, session stripesdk.CheckoutSession) (payments.Outcome, error) {
	switch kind {
	case domain.KindCheckoutCompleted:
		switch session.PaymentStatus {
		case stripesdk.CheckoutSessionPaymentStatusPaid,
			stripesdk.CheckoutSessionPaymentStatusNoPaymentRequired:
			return payments.OutcomeSucceeded, nil
		case stripesdk.CheckoutSessionPaymentStatusUnpaid:
			return payments.OutcomePending, nil
		default:
			return "", fmt.Errorf("%w: unknown payment_status %q on a completed session",
				app.ErrUnreadablePayload, session.PaymentStatus)
		}
	case domain.KindCheckoutPaymentSucceeded:
		return payments.OutcomeSucceeded, nil
	case domain.KindCheckoutPaymentFailed:
		return payments.OutcomeFailed, nil
	case domain.KindCheckoutExpired:
		return payments.OutcomeExpired, nil
	default:
		// The inbox stores unhandled kinds as skipped and never produces a
		// message for them, so reaching this means the message and the stored
		// event disagree.
		return "", fmt.Errorf("%w: event kind %q produces no transition",
			app.ErrUnreadablePayload, kind)
	}
}

var _ app.Interpreter = (*Session)(nil)
