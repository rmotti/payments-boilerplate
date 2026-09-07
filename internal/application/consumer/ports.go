// Package consumer contains the use case that applies the effects of a
// provider event. Receiving an event and acting on it are separate concerns:
// the webhooks package covers the first, this one covers the second.
package consumer

import (
	"context"
	"errors"
	"time"

	payments "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	webhooks "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

// Errors surfaced by the use case. They classify a failure, which is what
// decides whether the message is retried or dead-lettered.
var (
	// ErrIncompatibleAPIVersion means the provider signed the event with an
	// API version this adapter does not know how to read. Retrying cannot
	// help: the payload will not change shape.
	ErrIncompatibleAPIVersion = errors.New("provider event uses an incompatible API version")

	// ErrUnreadablePayload means the stored payload could not be parsed into
	// the object the event type promises.
	ErrUnreadablePayload = errors.New("provider payload could not be interpreted")

	// ErrMissingReference means the event carries no usable link back to a
	// local payment attempt. Without it there is nothing to transition.
	ErrMissingReference = errors.New("provider event carries no local reference")

	// ErrAggregateNotFound means the identifiers are well formed but name
	// rows that do not exist.
	ErrAggregateNotFound = errors.New("referenced payment aggregate does not exist")

	// ErrReferenceMismatch means the identifiers exist but do not belong
	// together: an attempt under a different payment, or a payment under a
	// different order.
	ErrReferenceMismatch = errors.New("provider event references an inconsistent aggregate")

	// ErrAmountMismatch means the provider settled an amount or currency that
	// is not the one recorded locally. Applying the effect would record a
	// payment that did not happen as described.
	ErrAmountMismatch = errors.New("provider amount does not match the local payment")

	// ErrStateConflict means the event contradicts a terminal state already
	// recorded. It is the DecisionConflict of the transition matrix.
	ErrStateConflict = errors.New("provider event contradicts a settled state")

	// ErrInvariantViolated means a transition the matrix approved changed no
	// row. Under the aggregate lock that must be impossible, so it signals
	// corruption rather than a race.
	ErrInvariantViolated = errors.New("approved transition matched no row")
)

// SessionOutcome is what an adapter reads out of a stored provider payload.
// It is provider-neutral: the adapter translates the provider's own
// vocabulary before the use case sees it.
type SessionOutcome struct {
	// OrderID, PaymentID and AttemptID come from the metadata this
	// application attached when it opened the session.
	OrderID   string
	PaymentID string
	AttemptID string

	// SessionID and PaymentIntentID are the provider's own references.
	SessionID       string
	PaymentIntentID string

	// Amount and Currency are what the provider says was charged. They are
	// compared against the local payment before any effect is applied.
	Amount   int64
	Currency string

	// Outcome is the financial result, in the vocabulary of the transition
	// matrix.
	Outcome payments.Outcome

	// OccurredAt is when the provider says the event happened.
	OccurredAt time.Time
}

// Interpreter reads a stored inbox entry and reports what it means.
//
// It receives the persisted event rather than raw bytes so the adapter can
// check the provider's event type against the payload it actually carries.
// This is where the API version compatibility check lives: the receiving
// endpoint deliberately skips it, because nothing there reads the provider's
// object graph, and an authentic event must be recorded even when the version
// differs. Here the graph is read, so the version has to be understood.
type Interpreter interface {
	Interpret(event webhooks.Event) (SessionOutcome, error)
	Provider() webhooks.Provider
}

// Aggregate is the locked local state an event acts upon.
type Aggregate struct {
	OrderID       string
	OrderStatus   string
	PaymentID     string
	PaymentStatus payments.Status
	Provider      string
	Amount        int64
	Currency      string
	AttemptID     string
	AttemptStatus payments.AttemptStatus
	// SessionID is the provider session already attached to the attempt. It
	// is empty when the attempt never reached the provider.
	SessionID string
}

// InboxEntry is the stored event plus the processing state the consumer keeps
// on it.
type InboxEntry struct {
	Event    webhooks.Event
	Attempts int
	Status   webhooks.Status
}

// Repository owns the transaction in which an event is applied.
//
// Load and Apply are deliberately not separate transactions. The repository
// opens one, locks the inbox row and then the whole aggregate in a fixed
// order, hands both to the caller's decision function, writes whatever it
// returns and commits. Splitting them would leave a window where the effect
// is recorded and the event is not marked processed, or the reverse.
type Repository interface {
	// Process runs decide inside one transaction, against state locked for
	// update. Whatever decide returns is applied and committed; an error
	// rolls the transaction back untouched.
	//
	// resolve turns the locked inbox entry into the identifiers of the
	// aggregate to lock. It is a function rather than a parameter because the
	// payload can only be interpreted after the entry is read, and the entry
	// can only be read inside the transaction.
	Process(ctx context.Context, eventID string, resolve Resolver, decide Decider) (Result, error)

	// RecordFailure notes an attempt that did not succeed, in its own short
	// transaction, after the processing transaction rolled back. It atomically
	// marks the inbox entry failed when the cause is terminal or this attempt
	// spends the retry budget, so the durable row cannot disagree with the DLQ.
	RecordFailure(
		ctx context.Context,
		eventID string,
		cause error,
		terminal bool,
		maxAttempts int,
	) (attempts int, failed bool, err error)
}

// Reference names the aggregate an event acts upon.
type Reference struct {
	OrderID   string
	PaymentID string
	AttemptID string
}

// Resolver reads the locked inbox entry and reports which aggregate to lock.
// It is where the provider payload is interpreted, so the repository never
// has to understand a provider's object graph.
type Resolver func(entry InboxEntry) (Reference, error)

// Decider receives the locked state and answers what to write. Returning an
// error aborts the transaction.
type Decider func(entry InboxEntry, aggregate Aggregate) (Effect, error)

// Effect is what the decider asks the repository to write. A zero-valued
// field leaves that entity untouched.
type Effect struct {
	AttemptStatus payments.AttemptStatus
	PaymentStatus payments.Status
	OrderPaid     bool
	// Note explains a no-op, and is stored so an operator can see why an
	// event produced nothing.
	Note string
}

// Result reports what applying one event did.
type Result struct {
	// Applied is false when the event was a no-op: already processed, or a
	// stale event that must not regress a state reached later.
	Applied bool
	// Note carries the reason of a no-op.
	Note string
}
