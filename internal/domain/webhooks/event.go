// Package webhooks holds the durable inbox of provider events and the outbox
// messages produced from them. Receiving an event and acting on it are separate
// concerns: this package covers only the first one.
package webhooks

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Provider identifies the external system that signed an event.
type Provider string

// Stripe is the first supported provider.
const Stripe Provider = "stripe"

// Kind is the provider-neutral meaning of a received event. The adapter
// translates the provider's own event name into one of these, so the published
// message never carries a provider's vocabulary.
type Kind string

// Event kinds understood by the application. Anything a provider sends that
// does not map to one of these is recorded and ignored.
const (
	KindUnknown                  Kind = ""
	KindCheckoutCompleted        Kind = "checkout.completed"
	KindCheckoutPaymentSucceeded Kind = "checkout.payment_succeeded"
	KindCheckoutPaymentFailed    Kind = "checkout.payment_failed"
	KindCheckoutExpired          Kind = "checkout.expired"
)

// Status is the processing state of an inbox entry.
type Status string

// Inbox statuses. Only Pending and Skipped are reachable while receiving; the
// remaining ones belong to the consumer.
const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusProcessed  Status = "processed"
	StatusFailed     Status = "failed"
	StatusSkipped    Status = "skipped"
)

// MessageStatus is the publication state of an outbox row.
type MessageStatus string

// Outbox statuses.
const (
	MessageStatusPending MessageStatus = "pending"
	MessagePublished     MessageStatus = "published"
	MessageStatusFailed  MessageStatus = "failed"
)

// SchemaVersion is the version of the message contract published to the broker.
// A breaking change bumps it or introduces a new routing key.
const SchemaVersion = 1

// routingKeyPrefix namespaces every message produced from a provider event.
const routingKeyPrefix = "payment.webhook."

// Validation errors.
var (
	ErrInvalidEvent   = errors.New("invalid webhook event")
	ErrInvalidMessage = errors.New("invalid outbox message")
)

// Event is one entry of the durable inbox.
type Event struct {
	ID string
	// Provider and ProviderEventID together deduplicate a redelivery.
	Provider        Provider
	ProviderEventID string
	// Type keeps the provider's own event name, for audit and support. The
	// domain meaning travels in Kind.
	Type       string
	Kind       Kind
	RawPayload []byte
	Payload    json.RawMessage
	Status     Status
	ReceivedAt time.Time
}

// Message is the outbox row created alongside an event. It references the
// event instead of copying it, so exactly one authoritative copy exists.
type Message struct {
	ID            string
	EventID       string
	Kind          Kind
	SchemaVersion int
	RoutingKey    string
	CorrelationID string
	OccurredAt    time.Time
	Status        MessageStatus
	CreatedAt     time.Time
}

// NewEvent records a verified provider event. A kind the application does not
// handle is still stored, as skipped, so the installation keeps an audit trail
// and notices when the provider starts sending something new.
func NewEvent(id string, provider Provider, providerEventID, eventType string, kind Kind, raw []byte, payload json.RawMessage, now time.Time) (Event, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(string(provider)) == "" ||
		strings.TrimSpace(providerEventID) == "" || strings.TrimSpace(eventType) == "" ||
		len(raw) == 0 || !json.Valid(payload) {
		return Event{}, ErrInvalidEvent
	}

	status := StatusPending
	if kind == KindUnknown {
		status = StatusSkipped
	}
	return Event{
		ID: id, Provider: provider, ProviderEventID: providerEventID,
		Type: eventType, Kind: kind, RawPayload: raw, Payload: payload,
		Status: status, ReceivedAt: now.UTC(),
	}, nil
}

// Handled reports whether the event produces an outbox message.
func (e Event) Handled() bool { return e.Kind != KindUnknown }

// NewMessage creates the outbox message for a handled event.
func NewMessage(id string, event Event, correlationID string, occurredAt, now time.Time) (Message, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(event.ID) == "" || !event.Handled() {
		return Message{}, ErrInvalidMessage
	}
	if occurredAt.IsZero() {
		occurredAt = now
	}
	return Message{
		ID: id, EventID: event.ID, Kind: event.Kind, SchemaVersion: SchemaVersion,
		RoutingKey: routingKeyPrefix + string(event.Kind), CorrelationID: correlationID,
		OccurredAt: occurredAt.UTC(), Status: MessageStatusPending, CreatedAt: now.UTC(),
	}, nil
}
