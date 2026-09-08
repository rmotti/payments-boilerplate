package consumer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	payments "github.com/rmotti/payments-boilerplate/internal/domain/payments"
)

// Disposition is what the consumer decides to do with a message once it has
// tried to apply it.
type Disposition int

// Message dispositions.
const (
	// DispositionDone means the effect is committed, or the event was a
	// deliberate no-op. The message is acknowledged and never comes back.
	DispositionDone Disposition = iota
	// DispositionRetry means the failure may end on its own. The message is
	// republished into a delay tier and acknowledged only after that
	// publication is confirmed.
	DispositionRetry
	// DispositionDead means the failure will not end on its own, or the retry
	// budget is spent. The message is republished to the dead-letter exchange
	// and acknowledged only after that publication is confirmed.
	DispositionDead
)

// String makes dispositions readable in logs.
func (d Disposition) String() string {
	switch d {
	case DispositionDone:
		return "done"
	case DispositionRetry:
		return "retry"
	case DispositionDead:
		return "dead"
	default:
		return "unknown"
	}
}

// Handling is the full answer for one message.
type Handling struct {
	Disposition Disposition
	// Attempts is how many times this event has now been tried. It comes from
	// the inbox, which is durable, rather than from a broker header.
	Attempts int
	// Cause is why the message is being retried or dead-lettered.
	Cause error
	// Note explains a no-op that succeeded.
	Note string
	// Applied reports whether a state transition was actually written.
	Applied bool
}

// Observer receives what the consumer would otherwise keep to itself.
type Observer interface {
	// Applied reports a committed transition.
	Applied(eventID string, effect Effect)
	// NoOp reports an event that produced nothing, and why.
	NoOp(eventID, note string)
	// Retrying reports a transient failure that will be tried again.
	Retrying(eventID string, attempts int, cause error)
	// DeadLettered reports a message that will not be tried again.
	DeadLettered(eventID string, attempts int, cause error)
}

// Config tunes how long the consumer keeps trying.
type Config struct {
	// MaxAttempts bounds how many times one event is tried before it is
	// dead-lettered. Unlike the relay, the consumer has a limit: a transient
	// relay failure is always infrastructure and ends on its own, while a
	// transient consumer failure may be an application bug that never does.
	MaxAttempts int
}

// DefaultMaxAttempts is used when configuration leaves the limit unset.
const DefaultMaxAttempts = 10

func (c Config) withDefaults() Config {
	if c.MaxAttempts < 2 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	return c
}

// Service applies the effects of received provider events.
type Service struct {
	repository  Repository
	interpreter Interpreter
	observer    Observer
	config      Config
	testHooks   TestHooks
}

// Option customizes deterministic dependencies in tests.
type Option func(*Service)

// TestHooks exposes the boundary immediately after Repository.Process has
// committed and before the broker adapter can acknowledge the delivery. It is
// installed only through an explicit constructor option; production
// configuration cannot enable fault injection.
type TestHooks struct {
	AfterCommit func(eventID string, result Result) error
}

// WithObserver reports what each message produced.
func WithObserver(observer Observer) Option {
	return func(s *Service) { s.observer = observer }
}

// WithTestHooks installs deterministic fault injection for tests. Application
// binaries never pass this option.
func WithTestHooks(hooks TestHooks) Option {
	return func(s *Service) { s.testHooks = hooks }
}

// NewService wires the consuming use case to its ports.
func NewService(repository Repository, interpreter Interpreter, config Config, opts ...Option) *Service {
	service := &Service{
		repository: repository, interpreter: interpreter, config: config.withDefaults(),
	}
	for _, option := range opts {
		option(service)
	}
	return service
}

// Config reports the effective configuration, defaults applied.
func (s *Service) Config() Config { return s.config }

// Handle applies one message and reports what should happen to it.
//
// It never returns an error for a failure it has classified: the disposition
// is the answer, and the caller turns it into an ack, a retry publication or a
// dead-letter publication. An error here means the classification itself could
// not be recorded, which is a reason to leave the message unacknowledged.
func (s *Service) Handle(ctx context.Context, eventID string) (Handling, error) {
	// outcome is captured by both closures below. Interpreting happens inside
	// the transaction, after the inbox row is locked, so a redelivery that
	// finds the event already processed never parses a payload at all.
	var outcome SessionOutcome
	var applied Effect

	result, err := s.repository.Process(ctx, eventID,
		func(entry InboxEntry) (Reference, error) {
			if entry.Event.Provider != s.interpreter.Provider() {
				return Reference{}, fmt.Errorf(
					"%w: event provider %s cannot be read by %s interpreter",
					ErrReferenceMismatch, entry.Event.Provider, s.interpreter.Provider())
			}
			interpreted, err := s.interpreter.Interpret(entry.Event)
			if err != nil {
				return Reference{}, err
			}
			outcome = interpreted
			return Reference{
				OrderID:   interpreted.OrderID,
				PaymentID: interpreted.PaymentID,
				AttemptID: interpreted.AttemptID,
			}, nil
		},
		func(_ InboxEntry, aggregate Aggregate) (Effect, error) {
			effect, err := decideEffect(outcome, aggregate, string(s.interpreter.Provider()))
			applied = effect
			return effect, err
		},
	)
	if err != nil {
		return s.classify(ctx, eventID, err)
	}
	if s.testHooks.AfterCommit != nil {
		if err := s.testHooks.AfterCommit(eventID, result); err != nil {
			return Handling{}, fmt.Errorf("after consumer commit: %w", err)
		}
	}

	if s.observer != nil {
		if result.Applied {
			s.observer.Applied(eventID, applied)
		} else {
			s.observer.NoOp(eventID, result.Note)
		}
	}
	return Handling{
		Disposition: DispositionDone,
		Applied:     result.Applied,
		Note:        result.Note,
	}, nil
}

// decideEffect checks the event against the locked aggregate and asks the
// transition matrix what to write.
//
// The checks run before the matrix on purpose. A mismatched amount or provider
// is not a state question at all: it means the event and the local row
// describe different things, and no transition is safe.
func decideEffect(outcome SessionOutcome, aggregate Aggregate, expectedProvider string) (Effect, error) {
	if !strings.EqualFold(aggregate.Provider, expectedProvider) {
		return Effect{}, fmt.Errorf(
			"%w: event provider %s, local payment provider is %s",
			ErrReferenceMismatch, expectedProvider, aggregate.Provider)
	}
	if outcome.Amount != aggregate.Amount ||
		!strings.EqualFold(outcome.Currency, aggregate.Currency) {
		return Effect{}, fmt.Errorf(
			"%w: provider settled %d %s, local payment is %d %s",
			ErrAmountMismatch, outcome.Amount, outcome.Currency,
			aggregate.Amount, aggregate.Currency)
	}
	// A session already attached to the attempt must be the one this event
	// talks about, or the event belongs to a different checkout.
	if aggregate.SessionID != "" && outcome.SessionID != "" &&
		aggregate.SessionID != outcome.SessionID {
		return Effect{}, fmt.Errorf(
			"%w: event carries session %s, attempt holds %s",
			ErrReferenceMismatch, outcome.SessionID, aggregate.SessionID)
	}

	transition := payments.Decide(outcome.Outcome, aggregate.PaymentStatus, aggregate.AttemptStatus)
	switch transition.Decision {
	case payments.DecisionApply:
		return Effect{
			AttemptStatus: transition.Attempt,
			PaymentStatus: transition.Payment,
			OrderPaid:     transition.OrderPaid,
			Note:          transition.Reason,
		}, nil
	case payments.DecisionNoOp:
		return Effect{Note: transition.Reason}, nil
	case payments.DecisionConflict:
		return Effect{}, fmt.Errorf("%w: %s", ErrStateConflict, transition.Reason)
	default:
		return Effect{}, fmt.Errorf("%w: unknown decision %s", ErrStateConflict, transition.Decision)
	}
}

// classify decides whether a failure is worth retrying, and records it.
//
// The default is transient, which is the safe direction: retrying a permanent
// failure wastes a few attempts and ends in the dead-letter queue anyway,
// while treating a transient failure as permanent abandons a financial event
// because the database blinked.
func (s *Service) classify(ctx context.Context, eventID string, cause error) (Handling, error) {
	terminal := isTerminal(cause)

	attempts, failed, recordErr := s.repository.RecordFailure(
		ctx, eventID, cause, terminal, s.config.MaxAttempts)
	if recordErr != nil {
		// Nothing was recorded, so the attempt count is unknown and the
		// message must not be acknowledged. Returning the error leaves it
		// unacknowledged and the broker redelivers it.
		return Handling{}, fmt.Errorf("record consumer failure: %w (cause: %w)", recordErr, cause)
	}

	// A failure that could not be counted, because there is no inbox row to
	// count it on, must not be retried: nothing will make the row appear, and
	// the attempt budget can never advance. Dead-lettering is the only way
	// such a message ever leaves the queue.
	if attempts == 0 {
		terminal = true
	} else {
		terminal = failed
	}

	// The budget is spent even for a transient failure. This is where the
	// consumer differs from the relay: a transient condition here may be a
	// bug that never resolves, and the dead-letter queue is where it becomes
	// visible instead of retrying forever.
	if terminal && !isTerminal(cause) && attempts >= s.config.MaxAttempts {
		cause = fmt.Errorf("giving up after %d attempts: %w", attempts, cause)
	}

	if terminal {
		if s.observer != nil {
			s.observer.DeadLettered(eventID, attempts, cause)
		}
		return Handling{Disposition: DispositionDead, Attempts: attempts, Cause: cause}, nil
	}

	if s.observer != nil {
		s.observer.Retrying(eventID, attempts, cause)
	}
	return Handling{Disposition: DispositionRetry, Attempts: attempts, Cause: cause}, nil
}

// terminalErrors never improve with another attempt: the stored bytes, the
// identifiers and the recorded states are all the same on every try.
var terminalErrors = []error{
	ErrIncompatibleAPIVersion,
	ErrUnreadablePayload,
	ErrMissingReference,
	ErrAggregateNotFound,
	ErrReferenceMismatch,
	ErrAmountMismatch,
	ErrStateConflict,
	ErrInvariantViolated,
}

// isTerminal reports whether retrying is pointless.
func isTerminal(err error) bool {
	for _, terminal := range terminalErrors {
		if errors.Is(err, terminal) {
			return true
		}
	}
	return false
}
