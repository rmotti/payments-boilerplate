// Package outbox contains the relay use case: it moves messages the API
// committed to PostgreSQL into the broker, and marks them published only after
// the broker confirms. Nothing else may mark a message as published.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

// Publisher is the narrow broker port. An implementation must only return nil
// after the broker confirmed the message, never merely after writing it.
type Publisher interface {
	Publish(ctx context.Context, message domain.Message) error
}

// Lease is a message claimed by this relay, together with the deadline by
// which its outcome must be recorded. Expires comes from the database, which
// is the only clock all instances share.
type Lease struct {
	Message  domain.Message
	Attempts int
	Expires  time.Time
}

// Repository leases due messages and records their outcome.
//
// Leasing and settling are separate calls on purpose: the publish happens
// between them, with no database transaction open, so a slow broker never
// holds locks. See ADR 0012.
type Repository interface {
	// Lease claims up to batchSize due messages for owner, reserving them for
	// leaseDuration. The deadline itself is computed by the database. Messages
	// whose holder never settled them before the deadline are due again, which
	// is how a relay that died releases its work.
	Lease(ctx context.Context, owner string, batchSize int, leaseDuration time.Duration) ([]Lease, error)
	// Published records a confirmed publication. It returns ErrLeaseLost when
	// the lease no longer belongs to owner.
	Published(ctx context.Context, owner, messageID string) error
	// Retry returns a message to the pool, due again after backoff.
	Retry(ctx context.Context, owner, messageID string, cause error, backoff time.Duration) error
	// Failed stops retrying a message whose error was classified permanent.
	Failed(ctx context.Context, owner, messageID string, cause error) error
}

// Result reports what one relay cycle did.
type Result struct {
	Leased    int
	Published int
	Retrying  int
	Failed    int
	// LeasesLost counts messages whose outcome could not be recorded because
	// the lease had expired. They are not published as far as this system
	// knows, and another instance will publish them again.
	LeasesLost int
}

// Empty reports whether the cycle found nothing to do.
func (r Result) Empty() bool { return r.Leased == 0 }

// Config tunes the relay loop.
type Config struct {
	// BatchSize bounds how many messages one cycle leases.
	BatchSize int
	// Interval is how long the relay waits after an idle cycle. It is the
	// floor on publication latency when the outbox is empty.
	Interval time.Duration
	// LeaseDuration is how long a claim is held. It must comfortably exceed
	// the time needed to publish a whole batch, because a lease that expires
	// mid-publish lets another relay pick the same message up.
	LeaseDuration time.Duration
	// Backoff spaces out retries of transient failures.
	Backoff Backoff
	// AlertAfterAttempts is how many failed attempts a message may accumulate
	// before the relay reports it as needing attention. It does NOT stop the
	// retries: a message is only abandoned when its error is permanent.
	AlertAfterAttempts int
}

// SettlementReserve is kept free at the end of every lease so the repository
// can record the current publication outcome before PostgreSQL expires it.
const SettlementReserve = 5 * time.Second

// Defaults applied when configuration leaves a field unset.
const (
	DefaultBatchSize          = 20
	DefaultInterval           = time.Second
	DefaultLeaseDuration      = 2 * time.Minute
	DefaultAlertAfterAttempts = 10
)

func (c Config) withDefaults() Config {
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = DefaultLeaseDuration
	}
	if c.AlertAfterAttempts <= 0 {
		c.AlertAfterAttempts = DefaultAlertAfterAttempts
	}
	c.Backoff = c.Backoff.withDefaults()
	return c
}

// Observer receives what the relay would otherwise keep to itself.
//
// An Observer may also implement CycleObserver to receive every publication
// and every cycle, not only the ones that need attention.
type Observer interface {
	// Stuck reports a message that keeps failing. The relay goes on retrying
	// it; this exists so an operator finds out before a customer does.
	Stuck(messageID string, attempts int, cause error)
	// Abandoned reports a message the relay will not try again.
	Abandoned(messageID string, attempts int, cause error)
	// LeaseLost reports an outcome that could not be recorded because the
	// lease had already expired. It usually means publishing is slower than
	// the configured lease.
	LeaseLost(messageID string, cause error)
}

// Outcome is what one publication attempt ended as.
type Outcome int

// Publication outcomes.
const (
	// OutcomePublished means the broker confirmed and the row was settled.
	OutcomePublished Outcome = iota
	// OutcomeRetrying means a transient failure scheduled the message again.
	OutcomeRetrying
	// OutcomeFailed means a permanent failure abandoned the message.
	OutcomeFailed
	// OutcomeLeaseLost means the outcome could not be recorded because the
	// lease had expired; another instance owns the message now.
	OutcomeLeaseLost
)

// String makes outcomes readable in logs and metrics.
func (o Outcome) String() string {
	switch o {
	case OutcomePublished:
		return "published"
	case OutcomeRetrying:
		return "retrying"
	case OutcomeFailed:
		return "failed"
	case OutcomeLeaseLost:
		return "lease_lost"
	default:
		return "unknown"
	}
}

// Publication reports one publish attempt after its outcome was recorded.
type Publication struct {
	Outcome Outcome
	// Attempts is the message's attempt count including this one.
	Attempts int
	// Duration is how long the broker took to confirm or refuse the message.
	// It does not include settling the row.
	Duration time.Duration
	// Cause is the publish error behind a retrying or failed outcome, and the
	// settlement error behind a lost lease. It is nil when published.
	Cause error
}

// CycleStage names where a cycle failed.
type CycleStage int

// Cycle stages.
const (
	// StageLease means the batch could not be claimed.
	StageLease CycleStage = iota + 1
	// StageSettlement means an outcome could not be recorded in PostgreSQL.
	StageSettlement
)

// String makes stages readable in logs and metrics.
func (s CycleStage) String() string {
	switch s {
	case StageLease:
		return "lease"
	case StageSettlement:
		return "settlement"
	default:
		return "unknown"
	}
}

// Cycle reports one RunOnce call.
type Cycle struct {
	Result   Result
	Duration time.Duration
	// Err is the cycle failure, if any, and Stage says where it happened.
	Err   error
	Stage CycleStage
}

// CycleObserver is the optional extension of Observer that receives every
// publication and every cycle. It is what a metrics implementation needs; a
// logging implementation usually does not want the volume.
type CycleObserver interface {
	PublicationSettled(publication Publication)
	CycleCompleted(cycle Cycle)
}

// Service publishes outbox messages until its context is cancelled.
type Service struct {
	repository Repository
	publisher  Publisher
	observers  []Observer
	owner      string
	config     Config
	testHooks  TestHooks
}

// Option customizes deterministic dependencies in tests.
type Option func(*Service)

// TestHooks exposes the narrow failure window between a broker confirm and
// settlement of the outbox row. It is deliberately available only through an
// explicit constructor option: production configuration and environment
// variables cannot enable fault injection.
//
// Returning an error from BeforeSettlement simulates the relay stopping after
// the broker accepted a message but before PostgreSQL recorded it. The lease
// remains unsettled and may be published again after it expires, which is the
// required at-least-once behaviour.
type TestHooks struct {
	BeforeSettlement func(message domain.Message) error
}

// WithObserver reports stuck and abandoned messages. It may be given more
// than once; observers are notified in the order they were added.
func WithObserver(observer Observer) Option {
	return func(s *Service) {
		if observer != nil {
			s.observers = append(s.observers, observer)
		}
	}
}

// WithTestHooks installs deterministic fault injection for tests. Application
// binaries never pass this option.
func WithTestHooks(hooks TestHooks) Option {
	return func(s *Service) { s.testHooks = hooks }
}

// NewService wires the relay to its ports. The owner identifies this relay
// instance in the lease, so a message can be traced to who is publishing it.
func NewService(repository Repository, publisher Publisher, owner string, config Config, opts ...Option) *Service {
	service := &Service{
		repository: repository, publisher: publisher, owner: owner,
		config: config.withDefaults(),
	}
	for _, option := range opts {
		option(service)
	}
	return service
}

// Config reports the effective configuration, defaults applied.
func (s *Service) Config() Config { return s.config }

// RunOnce leases one batch, publishes it and records each outcome.
func (s *Service) RunOnce(ctx context.Context) (Result, error) {
	started := time.Now()
	result, stage, err := s.runOnce(ctx)
	s.eachCycleObserver(func(observer CycleObserver) {
		observer.CycleCompleted(Cycle{
			Result: result, Duration: time.Since(started), Err: err, Stage: stage,
		})
	})
	return result, err
}

func (s *Service) runOnce(ctx context.Context) (Result, CycleStage, error) {
	// Start a monotonic, conservative clock before PostgreSQL creates the lease.
	// The database deadline will therefore be slightly later than this local
	// window regardless of clock skew between the database and this process.
	publicationWindow := s.config.LeaseDuration - SettlementReserve
	publicationCtx, cancelPublication := context.WithTimeout(ctx, publicationWindow)
	defer cancelPublication()

	leases, err := s.repository.Lease(publicationCtx, s.owner, s.config.BatchSize, s.config.LeaseDuration)
	if err != nil {
		return Result{}, StageLease, fmt.Errorf("lease outbox batch: %w", err)
	}

	result := Result{Leased: len(leases)}
	for _, lease := range leases {
		// Stop early on shutdown: the remaining leases simply expire and are
		// picked up again, which is cheaper than racing a cancelled context.
		if ctx.Err() != nil || publicationCtx.Err() != nil {
			return result, 0, nil
		}
		outcome, err := s.publishOne(ctx, publicationCtx, lease)
		if err != nil {
			return result, StageSettlement, err
		}
		switch outcome {
		case OutcomePublished:
			result.Published++
		case OutcomeRetrying:
			result.Retrying++
		case OutcomeFailed:
			result.Failed++
		case OutcomeLeaseLost:
			result.LeasesLost++
		}
	}
	return result, 0, nil
}

func (s *Service) publishOne(ctx, publicationCtx context.Context, lease Lease) (Outcome, error) {
	started := time.Now()
	// publicationCtx is monotonic and ends before the database lease. Interpreting
	// the absolute PostgreSQL timestamp with the worker's wall clock would bring
	// clock skew back into the decision this deadline is meant to protect.
	publishErr := s.publisher.Publish(publicationCtx, lease.Message)
	publishDuration := time.Since(started)
	attempts := lease.Attempts + 1

	outcome, cause, err := s.settle(ctx, lease, publishErr, attempts)
	if err != nil {
		return 0, err
	}
	s.eachCycleObserver(func(observer CycleObserver) {
		observer.PublicationSettled(Publication{
			Outcome: outcome, Attempts: attempts, Duration: publishDuration, Cause: cause,
		})
	})
	return outcome, nil
}

// settle records the outcome of one publish attempt. The returned cause is
// what explains a non-published outcome; the error means the cycle itself
// failed and nothing was recorded.
func (s *Service) settle(ctx context.Context, lease Lease, publishErr error, attempts int) (Outcome, error, error) {
	if publishErr == nil {
		if s.testHooks.BeforeSettlement != nil {
			if err := s.testHooks.BeforeSettlement(lease.Message); err != nil {
				return 0, nil, fmt.Errorf("before outbox settlement: %w", err)
			}
		}
		// Settling uses the parent context: the publication already happened,
		// and recording it matters even if the lease is about to expire.
		if err := s.repository.Published(ctx, s.owner, lease.Message.ID); err != nil {
			return s.leaseOutcome(lease, err, "mark published")
		}
		return OutcomePublished, nil, nil
	}

	// Only a classified permanent error abandons a message. Broker downtime,
	// timeouts and nacks are transient by construction and keep retrying, so
	// an outage can never discard a financial event.
	if IsPermanent(publishErr) {
		if err := s.repository.Failed(ctx, s.owner, lease.Message.ID, publishErr); err != nil {
			return s.leaseOutcome(lease, err, "mark failed")
		}
		s.eachObserver(func(observer Observer) {
			observer.Abandoned(lease.Message.ID, attempts, publishErr)
		})
		return OutcomeFailed, publishErr, nil
	}

	if err := s.repository.Retry(ctx, s.owner, lease.Message.ID, publishErr,
		s.config.Backoff.Delay(lease.Attempts)); err != nil {
		return s.leaseOutcome(lease, err, "mark retryable")
	}
	if attempts >= s.config.AlertAfterAttempts {
		s.eachObserver(func(observer Observer) {
			observer.Stuck(lease.Message.ID, attempts, publishErr)
		})
	}
	return OutcomeRetrying, publishErr, nil
}

// leaseOutcome separates a lost lease from a real storage failure. A lost
// lease is not an error of the cycle: the message simply belongs to someone
// else now, and will be published again. It must still be visible, because it
// means publishing is outrunning the configured lease.
func (s *Service) leaseOutcome(lease Lease, err error, action string) (Outcome, error, error) {
	if errors.Is(err, ErrLeaseLost) {
		s.eachObserver(func(observer Observer) {
			observer.LeaseLost(lease.Message.ID, err)
		})
		return OutcomeLeaseLost, err, nil
	}
	return 0, nil, fmt.Errorf("%s: %w", action, err)
}

func (s *Service) eachObserver(notify func(Observer)) {
	for _, observer := range s.observers {
		notify(observer)
	}
}

func (s *Service) eachCycleObserver(notify func(CycleObserver)) {
	for _, observer := range s.observers {
		if extended, ok := observer.(CycleObserver); ok {
			notify(extended)
		}
	}
}

// Run relays until ctx is cancelled.
//
// A cycle that published something is followed immediately by another, so a
// backlog drains at the speed of the broker rather than of the timer. A cycle
// that only retried does wait: those messages are not due yet anyway, and
// spinning would burn their backoff in milliseconds.
func (s *Service) Run(ctx context.Context, onError func(error)) error {
	timer := time.NewTimer(s.config.Interval)
	defer timer.Stop()

	for {
		result, err := s.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if onError != nil {
				onError(fmt.Errorf("relay cycle: %w", err))
			}
		}

		if err == nil && result.Published > 0 {
			select {
			case <-ctx.Done():
				return nil
			default:
				continue
			}
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(s.config.Interval)
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}
