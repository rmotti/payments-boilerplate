package consumer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	payments "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	webhooks "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

// fakeRepository runs the resolver and decider against fixed state, the way
// the real one does inside its transaction, without a database.
type fakeRepository struct {
	entry     InboxEntry
	aggregate Aggregate

	// processErr replaces the whole transaction with a failure.
	processErr error
	// alreadyProcessed short-circuits before the aggregate is locked.
	alreadyProcessed bool

	recordErr error
	attempts  int

	gotEffect   Effect
	gotTerminal bool
	recorded    bool
}

func (f *fakeRepository) Process(
	_ context.Context, _ string, resolve Resolver, decide Decider,
) (Result, error) {
	if f.processErr != nil {
		return Result{}, f.processErr
	}
	if f.alreadyProcessed {
		return Result{Applied: false, Note: "event was already processed"}, nil
	}
	entry := f.entry
	if entry.Event.Provider == "" {
		entry.Event.Provider = webhooks.Stripe
	}
	if _, err := resolve(entry); err != nil {
		return Result{}, err
	}
	effect, err := decide(entry, f.aggregate)
	if err != nil {
		return Result{}, err
	}
	f.gotEffect = effect
	applied := effect.AttemptStatus != "" || effect.PaymentStatus != "" || effect.OrderPaid
	return Result{Applied: applied, Note: effect.Note}, nil
}

func (f *fakeRepository) RecordFailure(
	_ context.Context,
	_ string,
	_ error,
	terminal bool,
	maxAttempts int,
) (int, bool, error) {
	f.recorded = true
	if f.recordErr != nil {
		return 0, false, f.recordErr
	}
	f.attempts++
	f.gotTerminal = terminal || f.attempts >= maxAttempts
	return f.attempts, f.gotTerminal, nil
}

// fakeInterpreter returns a fixed reading of the payload.
type fakeInterpreter struct {
	outcome SessionOutcome
	err     error
}

func (f fakeInterpreter) Interpret(webhooks.Event) (SessionOutcome, error) {
	return f.outcome, f.err
}
func (f fakeInterpreter) Provider() webhooks.Provider { return webhooks.Stripe }

func paidAggregate() Aggregate {
	return Aggregate{
		OrderID: "ord_1", OrderStatus: "pending",
		PaymentID: "pay_1", PaymentStatus: payments.StatusPending,
		Provider: "stripe", Amount: 10000, Currency: "BRL",
		AttemptID: "pat_1", AttemptStatus: payments.AttemptStatusPending,
	}
}

func succeededOutcome() SessionOutcome {
	return SessionOutcome{
		OrderID: "ord_1", PaymentID: "pay_1", AttemptID: "pat_1",
		SessionID: "cs_1", Amount: 10000, Currency: "BRL",
		Outcome: payments.OutcomeSucceeded,
	}
}

func TestHandleAppliesASuccessfulPayment(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{aggregate: paidAggregate()}
	service := NewService(repository, fakeInterpreter{outcome: succeededOutcome()}, Config{})

	got, err := service.Handle(context.Background(), "evt_1")
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Disposition != DispositionDone || !got.Applied {
		t.Fatalf("Handle() = %#v, want an applied done", got)
	}
	want := Effect{
		AttemptStatus: payments.AttemptStatusSucceeded,
		PaymentStatus: payments.StatusSucceeded,
		OrderPaid:     true,
	}
	if repository.gotEffect != want {
		t.Errorf("effect = %#v, want %#v", repository.gotEffect, want)
	}
}

// A redelivery must not reinterpret the payload or touch the aggregate.
func TestHandleTreatsAProcessedEventAsDone(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{alreadyProcessed: true}
	interpreter := fakeInterpreter{err: errors.New("must not be called")}
	service := NewService(repository, interpreter, Config{})

	got, err := service.Handle(context.Background(), "evt_1")
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Disposition != DispositionDone || got.Applied {
		t.Errorf("Handle() = %#v, want an unapplied done", got)
	}
}

// Repository.Process owns the transaction and returns only after commit. A
// process loss in the following window must surface as an error so the broker
// adapter leaves the original delivery unacknowledged. Its redelivery then
// observes the already-processed inbox row and becomes a no-op.
func TestHandleLeavesCommitBeforeAckWindowSafeForRedelivery(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{aggregate: paidAggregate()}
	var injected bool
	service := NewService(repository, fakeInterpreter{outcome: succeededOutcome()}, Config{},
		WithTestHooks(TestHooks{AfterCommit: func(_ string, result Result) error {
			if !result.Applied {
				t.Fatal("hook ran before the committed effect was reported")
			}
			injected = true
			return errors.New("worker stopped before ack")
		}}))

	if _, err := service.Handle(context.Background(), "evt_1"); err == nil {
		t.Fatal("Handle() error = nil, want the delivery left unacknowledged")
	}
	if !injected || repository.gotEffect.PaymentStatus != payments.StatusSucceeded {
		t.Fatal("fault did not occur after the committed effect")
	}

	// Model what the real repository returns after the committed delivery is
	// redelivered: no payload interpretation and no second state transition.
	repository.alreadyProcessed = true
	service = NewService(repository,
		fakeInterpreter{err: errors.New("redelivery must not be interpreted")}, Config{})
	got, err := service.Handle(context.Background(), "evt_1")
	if err != nil {
		t.Fatalf("redelivery Handle() error = %v", err)
	}
	if got.Disposition != DispositionDone || got.Applied {
		t.Fatalf("redelivery = %#v, want an acknowledged no-op", got)
	}
}

// A stale event must be acknowledged, not retried: it is correct behaviour,
// not a failure.
func TestHandleAcknowledgesAStaleEvent(t *testing.T) {
	t.Parallel()

	aggregate := paidAggregate()
	aggregate.PaymentStatus = payments.StatusSucceeded
	aggregate.AttemptStatus = payments.AttemptStatusSucceeded

	outcome := succeededOutcome()
	outcome.Outcome = payments.OutcomeExpired

	repository := &fakeRepository{aggregate: aggregate}
	service := NewService(repository, fakeInterpreter{outcome: outcome}, Config{})

	got, err := service.Handle(context.Background(), "evt_1")
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Disposition != DispositionDone || got.Applied {
		t.Errorf("Handle() = %#v, want an unapplied done", got)
	}
	if repository.recorded {
		t.Error("a stale event must not be recorded as a failure")
	}
}

func TestHandleDeadLettersTerminalFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		outcome     SessionOutcome
		aggregate   Aggregate
		interpreter error
	}{
		{
			name: "amount does not match the local payment",
			outcome: func() SessionOutcome {
				o := succeededOutcome()
				o.Amount = 999
				return o
			}(),
			aggregate: paidAggregate(),
		},
		{
			name: "currency does not match the local payment",
			outcome: func() SessionOutcome {
				o := succeededOutcome()
				o.Currency = "USD"
				return o
			}(),
			aggregate: paidAggregate(),
		},
		{
			name:    "event belongs to a different session",
			outcome: succeededOutcome(),
			aggregate: func() Aggregate {
				a := paidAggregate()
				a.SessionID = "cs_other"
				return a
			}(),
		},
		{
			name:    "event belongs to a different provider",
			outcome: succeededOutcome(),
			aggregate: func() Aggregate {
				a := paidAggregate()
				a.Provider = "another-provider"
				return a
			}(),
		},
		{
			name:    "success contradicts a failed payment",
			outcome: succeededOutcome(),
			aggregate: func() Aggregate {
				a := paidAggregate()
				a.PaymentStatus = payments.StatusFailed
				a.AttemptStatus = payments.AttemptStatusFailed
				return a
			}(),
		},
		{
			name:        "payload cannot be interpreted",
			aggregate:   paidAggregate(),
			interpreter: fmt.Errorf("%w: broken", ErrUnreadablePayload),
		},
		{
			name:        "api version is not supported",
			aggregate:   paidAggregate(),
			interpreter: fmt.Errorf("%w: 2019-01-01", ErrIncompatibleAPIVersion),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			repository := &fakeRepository{aggregate: test.aggregate}
			service := NewService(repository,
				fakeInterpreter{outcome: test.outcome, err: test.interpreter}, Config{})

			got, err := service.Handle(context.Background(), "evt_1")
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if got.Disposition != DispositionDead {
				t.Errorf("Disposition = %s, want dead (cause: %v)", got.Disposition, got.Cause)
			}
			if !repository.gotTerminal {
				t.Error("failure must be recorded as terminal")
			}
		})
	}
}

func TestHandleRejectsAnEventFromAnotherProvider(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{
		entry:     InboxEntry{Event: webhooks.Event{Provider: webhooks.Provider("another-provider")}},
		aggregate: paidAggregate(),
	}
	service := NewService(repository, fakeInterpreter{outcome: succeededOutcome()}, Config{})

	got, err := service.Handle(context.Background(), "evt_1")
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Disposition != DispositionDead || !repository.gotTerminal {
		t.Errorf("Handle() = %#v, terminal = %t, want terminal dead-letter", got, repository.gotTerminal)
	}
}

// A database failure is transient by default: the safe direction is to retry.
func TestHandleRetriesUnclassifiedFailures(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{processErr: errors.New("connection reset by peer")}
	service := NewService(repository, fakeInterpreter{outcome: succeededOutcome()}, Config{MaxAttempts: 5})

	got, err := service.Handle(context.Background(), "evt_1")
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Disposition != DispositionRetry {
		t.Errorf("Disposition = %s, want retry", got.Disposition)
	}
	if repository.gotTerminal {
		t.Error("an unclassified failure must not be recorded as terminal")
	}
}

// The retry budget is what separates the consumer from the relay: a transient
// failure here may be a bug that never ends, so it eventually stops.
func TestHandleDeadLettersOnceTheBudgetIsSpent(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{
		processErr: errors.New("connection reset by peer"),
		attempts:   2,
	}
	service := NewService(repository, fakeInterpreter{outcome: succeededOutcome()}, Config{MaxAttempts: 3})

	got, err := service.Handle(context.Background(), "evt_1")
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Disposition != DispositionDead {
		t.Errorf("Disposition = %s, want dead after the budget is spent", got.Disposition)
	}
	if got.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", got.Attempts)
	}
	if !repository.gotTerminal {
		t.Error("the attempt that spends the budget must be recorded as failed")
	}
}

// If the failure itself cannot be recorded, the attempt count is unknown. The
// message must stay unacknowledged rather than be acknowledged on a guess.
func TestHandleRefusesToDecideWhenTheFailureCannotBeRecorded(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{
		processErr: errors.New("connection reset by peer"),
		recordErr:  errors.New("database is down"),
	}
	service := NewService(repository, fakeInterpreter{outcome: succeededOutcome()}, Config{})

	if _, err := service.Handle(context.Background(), "evt_1"); err == nil {
		t.Fatal("Handle() error = nil, want the message left unacknowledged")
	}
}

// An unpaid completed session is the Pix path: processing, and the attempt
// stays open for the asynchronous event that settles it.
func TestHandleKeepsTheAttemptOpenForLateConfirmation(t *testing.T) {
	t.Parallel()

	outcome := succeededOutcome()
	outcome.Outcome = payments.OutcomePending

	repository := &fakeRepository{aggregate: paidAggregate()}
	service := NewService(repository, fakeInterpreter{outcome: outcome}, Config{})

	if _, err := service.Handle(context.Background(), "evt_1"); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	want := Effect{PaymentStatus: payments.StatusProcessing}
	if repository.gotEffect != want {
		t.Errorf("effect = %#v, want %#v", repository.gotEffect, want)
	}
}

// A message naming an event that does not exist can never advance an attempt
// count, so it must be dead-lettered rather than retried forever against a row
// that will never appear.
func TestHandleDeadLettersAnEventThatCannotBeCounted(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{
		processErr: fmt.Errorf("%w: evt_missing", ErrAggregateNotFound),
		attempts:   -1, // RecordFailure returns 0: nothing to count on.
	}
	service := NewService(repository, fakeInterpreter{outcome: succeededOutcome()}, Config{})

	got, err := service.Handle(context.Background(), "evt_missing")
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Disposition != DispositionDead {
		t.Errorf("Disposition = %s, want dead for an uncountable event", got.Disposition)
	}
}
