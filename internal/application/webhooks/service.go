// Package webhooks contains the use case that durably accepts a provider event.
// It deliberately produces no business effect: the event is verified, written to
// the inbox together with its outbox message, and nothing else happens inside
// the request. The consumer applies the effects later.
package webhooks

import (
	"context"
	"errors"
	"fmt"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

// storeTimeout bounds the receiving transaction. It is deliberately shorter
// than the provider's own timeout: a request left hanging produces the same
// redelivery a 500 would, but without a local record of what happened.
const storeTimeout = 3 * time.Second

// Errors surfaced by the use case.
var (
	// ErrInvalidSignature covers an absent, malformed, forged or expired
	// signature. The causes are not distinguished on purpose: from inside the
	// request an attacker and a misconfigured secret look identical.
	ErrInvalidSignature = errors.New("webhook signature could not be verified")

	// ErrPayloadTooLarge means the body exceeded the endpoint limit, so the
	// signature could never be checked. Unlike a bad signature, a redelivery
	// succeeds once the limit is corrected.
	ErrPayloadTooLarge = errors.New("webhook payload exceeds the accepted size")
)

// ProviderEvent is what an adapter returns after verifying a signature.
type ProviderEvent struct {
	// ID is the provider's own event identifier and the deduplication key.
	ID string
	// Type is the provider's event name, kept for audit.
	Type string
	// Kind is the provider-neutral meaning, or KindUnknown when the
	// application does not handle this event.
	Kind       domain.Kind
	OccurredAt time.Time
	Payload    []byte
}

// Verifier is the narrow provider port. It must validate the signature over
// the exact bytes received, before any deserialization the application does.
type Verifier interface {
	Verify(rawBody []byte, signature string) (ProviderEvent, error)
	Provider() domain.Provider
}

// Repository writes the inbox entry and, when the event is handled, its outbox
// message in the same transaction. It reports whether the event was new.
type Repository interface {
	// StoreIgnored persists an event the application does not handle. No
	// outbox message is produced.
	StoreIgnored(ctx context.Context, event domain.Event) (bool, error)
	// StorePending persists an event and its message atomically.
	StorePending(ctx context.Context, event domain.Event, message domain.Message) (bool, error)
}

// Outcome reports what receiving the event produced.
type Outcome string

// Receive outcomes.
const (
	// OutcomeAccepted means the event was stored and queued for processing.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeDuplicate means the event had already been received.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeIgnored means the event was stored but produces no message.
	OutcomeIgnored Outcome = "ignored"
)

// ReceiveInput carries the untouched request bytes and signature header, or
// the transport failure that prevented the complete body from being read.
type ReceiveInput struct {
	RawBody       []byte
	Signature     string
	CorrelationID string
	// ReadError reports that the transport could not obtain the complete raw
	// body. Receive still accepts the failed input so its observer accounts for
	// the request, but verification and persistence are never attempted.
	ReadError error
}

// Receipt reports one Receive call to an Observer. It carries no identifier
// and no payload on purpose: everything in it is safe to aggregate.
type Receipt struct {
	Provider domain.Provider
	// Kind is the provider-neutral meaning of the event, or KindUnknown when
	// the application does not handle it or the request could not be verified.
	Kind domain.Kind
	// Outcome is what receiving produced. It is empty when Err is set.
	Outcome  Outcome
	Duration time.Duration
	// Err is the failure that prevented the event from being stored.
	Err error
}

// Observer receives one Receipt per Receive call.
type Observer interface {
	Received(receipt Receipt)
}

// Service accepts provider events durably.
type Service struct {
	verifier     Verifier
	repository   Repository
	observer     Observer
	now          func() time.Time
	newEventID   func() (string, error)
	newMessageID func() (string, error)
}

// Option customizes deterministic dependencies in tests.
type Option func(*Service)

// WithClock replaces the wall clock.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithIDGenerators replaces both local identifier generators.
func WithIDGenerators(eventID, messageID func() (string, error)) Option {
	return func(s *Service) {
		s.newEventID = eventID
		s.newMessageID = messageID
	}
}

// WithObserver reports every receive outcome.
func WithObserver(observer Observer) Option {
	return func(s *Service) { s.observer = observer }
}

// NewService wires the receiving use case to its ports.
func NewService(verifier Verifier, repository Repository, opts ...Option) *Service {
	service := &Service{
		verifier: verifier, repository: repository, now: time.Now,
		newEventID: domain.NewEventID, newMessageID: domain.NewMessageID,
	}
	for _, option := range opts {
		option(service)
	}
	return service
}

// Receive verifies and durably stores one provider event.
func (s *Service) Receive(ctx context.Context, input ReceiveInput) (Outcome, error) {
	started := time.Now()
	outcome, kind, err := s.receive(ctx, input)
	if s.observer != nil {
		s.observer.Received(Receipt{
			Provider: s.verifier.Provider(), Kind: kind, Outcome: outcome,
			Duration: time.Since(started), Err: err,
		})
	}
	return outcome, err
}

func (s *Service) receive(ctx context.Context, input ReceiveInput) (Outcome, domain.Kind, error) {
	if input.ReadError != nil {
		return "", domain.KindUnknown, input.ReadError
	}
	providerEvent, err := s.verifier.Verify(input.RawBody, input.Signature)
	if err != nil {
		return "", domain.KindUnknown, fmt.Errorf("%w: %w", ErrInvalidSignature, err)
	}
	kind := providerEvent.Kind

	eventID, err := s.newEventID()
	if err != nil {
		return "", kind, err
	}
	now := s.now()
	event, err := domain.NewEvent(
		eventID, s.verifier.Provider(), providerEvent.ID, providerEvent.Type,
		providerEvent.Kind, input.RawBody, providerEvent.Payload, now,
	)
	if err != nil {
		// The signature already proved the bytes came from the provider, so a
		// malformed event here is not a client error.
		return "", kind, fmt.Errorf("build webhook event: %w", err)
	}

	storeCtx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	if !event.Handled() {
		stored, err := s.repository.StoreIgnored(storeCtx, event)
		if err != nil {
			return "", kind, fmt.Errorf("store ignored webhook event: %w", err)
		}
		if !stored {
			return OutcomeDuplicate, kind, nil
		}
		return OutcomeIgnored, kind, nil
	}

	messageID, err := s.newMessageID()
	if err != nil {
		return "", kind, err
	}
	message, err := domain.NewMessage(messageID, event, input.CorrelationID, providerEvent.OccurredAt, now)
	if err != nil {
		return "", kind, fmt.Errorf("build outbox message: %w", err)
	}

	stored, err := s.repository.StorePending(storeCtx, event, message)
	if err != nil {
		return "", kind, fmt.Errorf("store webhook event: %w", err)
	}
	if !stored {
		return OutcomeDuplicate, kind, nil
	}
	return OutcomeAccepted, kind, nil
}
