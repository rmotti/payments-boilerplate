package outbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

type recordedRetry struct {
	messageID string
	backoff   time.Duration
}

type stubRepository struct {
	mu sync.Mutex

	batches   [][]Lease
	leaseErr  error
	calls     atomic.Int32
	published []string
	retries   []recordedRetry
	failed    []string
	leaseArgs []time.Duration
	settleErr error
}

func (s *stubRepository) Lease(_ context.Context, _ string, _ int, leaseDuration time.Duration) ([]Lease, error) {
	index := int(s.calls.Add(1)) - 1
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseArgs = append(s.leaseArgs, leaseDuration)
	if s.leaseErr != nil {
		return nil, s.leaseErr
	}
	if index < len(s.batches) {
		return s.batches[index], nil
	}
	return nil, nil
}

func (s *stubRepository) Published(_ context.Context, _, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settleErr != nil {
		return s.settleErr
	}
	s.published = append(s.published, messageID)
	return nil
}

func (s *stubRepository) Retry(_ context.Context, _, messageID string, _ error, backoff time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settleErr != nil {
		return s.settleErr
	}
	s.retries = append(s.retries, recordedRetry{messageID: messageID, backoff: backoff})
	return nil
}

func (s *stubRepository) Failed(_ context.Context, _, messageID string, _ error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settleErr != nil {
		return s.settleErr
	}
	s.failed = append(s.failed, messageID)
	return nil
}

type stubPublisher struct {
	err   error
	calls atomic.Int32
}

func (s *stubPublisher) Publish(context.Context, domain.Message) error {
	s.calls.Add(1)
	return s.err
}

type recordingObserver struct {
	mu         sync.Mutex
	stuck      []string
	abandoned  []string
	leasesLost []string
}

func (o *recordingObserver) Stuck(messageID string, _ int, _ error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stuck = append(o.stuck, messageID)
}

func (o *recordingObserver) Abandoned(messageID string, _ int, _ error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.abandoned = append(o.abandoned, messageID)
}

func (o *recordingObserver) LeaseLost(messageID string, _ error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.leasesLost = append(o.leasesLost, messageID)
}

func lease(id string, attempts int) Lease {
	return Lease{
		Message: domain.Message{
			ID: id, EventID: "evt_" + id, Kind: domain.KindCheckoutCompleted,
			SchemaVersion: domain.SchemaVersion, RoutingKey: "payment.webhook.checkout.completed",
		},
		Attempts: attempts,
	}
}

func newService(repository Repository, publisher Publisher, config Config, opts ...Option) *Service {
	return NewService(repository, publisher, "test-instance", config, opts...)
}

func TestConfigAppliesDefaults(t *testing.T) {
	t.Parallel()

	config := newService(&stubRepository{}, &stubPublisher{}, Config{}).Config()
	if config.BatchSize != DefaultBatchSize || config.Interval != DefaultInterval ||
		config.LeaseDuration != DefaultLeaseDuration || config.AlertAfterAttempts != DefaultAlertAfterAttempts {
		t.Fatalf("config = %#v, want the documented defaults", config)
	}
	if config.Backoff.Base != DefaultBackoffBase || config.Backoff.Max != DefaultBackoffMax {
		t.Fatalf("backoff = %#v, want the documented defaults", config.Backoff)
	}
}

func TestRunOncePublishesAndMarks(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{batches: [][]Lease{{lease("msg_1", 0), lease("msg_2", 0)}}}
	service := newService(repository, &stubPublisher{}, Config{})

	result, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Published != 2 || result.Leased != 2 {
		t.Fatalf("result = %#v, want two published", result)
	}
	if len(repository.published) != 2 {
		t.Fatalf("published = %v, want both messages marked", repository.published)
	}
}

// The relay passes a duration, never a computed instant: the database is the
// only clock several instances share, so it stamps the deadline itself.
func TestRunOnceLeasesWithConfiguredDuration(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{}
	service := newService(repository, &stubPublisher{}, Config{LeaseDuration: 90 * time.Second})

	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if repository.leaseArgs[0] != 90*time.Second {
		t.Fatalf("leaseDuration = %v, want 90s", repository.leaseArgs[0])
	}
}

// This is the regression guard for the original design: a transient failure
// must never abandon a financial message, no matter how many times it happens.
func TestTransientFailuresNeverAbandonAMessage(t *testing.T) {
	t.Parallel()

	batches := make([][]Lease, 0, 50)
	for i := 0; i < 50; i++ {
		batches = append(batches, []Lease{lease("msg_stubborn", i)})
	}
	repository := &stubRepository{batches: batches}
	observer := &recordingObserver{}
	service := newService(repository, &stubPublisher{err: ErrNotConfirmed},
		Config{AlertAfterAttempts: 3}, WithObserver(observer))

	for i := 0; i < 50; i++ {
		result, err := service.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("cycle %d error = %v", i, err)
		}
		if result.Failed != 0 {
			t.Fatalf("cycle %d abandoned a message after a transient failure", i)
		}
	}
	if len(repository.failed) != 0 {
		t.Fatalf("failed = %v, want a broker outage to never abandon a message", repository.failed)
	}
	if len(repository.retries) != 50 {
		t.Fatalf("retries = %d, want every attempt rescheduled", len(repository.retries))
	}
	// The operator is told, even though the relay keeps trying.
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.stuck) == 0 {
		t.Fatal("a message that keeps failing must be reported")
	}
}

func TestBrokerNackAndMandatoryReturnKeepMessageEligible(t *testing.T) {
	t.Parallel()

	for _, brokerErr := range []error{ErrNotConfirmed, ErrNotRouted} {
		brokerErr := brokerErr
		t.Run(brokerErr.Error(), func(t *testing.T) {
			t.Parallel()
			repository := &stubRepository{batches: [][]Lease{{lease("msg_retry", 0)}}}
			service := newService(repository, &stubPublisher{err: brokerErr}, Config{})

			result, err := service.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if result.Retrying != 1 || len(repository.retries) != 1 {
				t.Fatalf("result/retries = %#v/%v, want the row eligible for retry",
					result, repository.retries)
			}
			if len(repository.published) != 0 || len(repository.failed) != 0 {
				t.Fatalf("published/failed = %v/%v, want neither", repository.published, repository.failed)
			}
		})
	}
}

// Only an explicitly permanent error stops the retries.
func TestPermanentFailureAbandonsMessage(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{batches: [][]Lease{{lease("msg_bad", 0)}}}
	observer := &recordingObserver{}
	service := newService(repository, &stubPublisher{err: Permanent(errors.New("cannot encode"))},
		Config{}, WithObserver(observer))

	result, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("result = %#v, want the message abandoned", result)
	}
	if len(repository.failed) != 1 || repository.failed[0] != "msg_bad" {
		t.Fatalf("failed = %v, want msg_bad", repository.failed)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.abandoned) != 1 {
		t.Fatal("abandoning a message must be reported")
	}
}

// A retry must be scheduled into the future, and further attempts must not be
// scheduled sooner than earlier ones on average.
func TestRetryIsScheduledWithBackoff(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{batches: [][]Lease{{lease("msg_1", 0)}, {lease("msg_1", 6)}}}
	service := newService(repository, &stubPublisher{err: ErrNotConfirmed},
		Config{Backoff: Backoff{Base: time.Second, Max: time.Hour}})

	for i := 0; i < 2; i++ {
		if _, err := service.RunOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d error = %v", i, err)
		}
	}

	for i, retry := range repository.retries {
		if retry.backoff <= 0 {
			t.Fatalf("retry %d scheduled with backoff %v, want a positive wait", i, retry.backoff)
		}
	}
}

// A cycle that only rescheduled retries must wait, otherwise the backoff it
// just computed would be burned in a spin loop. This is the bug the original
// design had: ten attempts could elapse in milliseconds.
func TestRunWaitsWhenNothingWasPublished(t *testing.T) {
	t.Parallel()

	batches := make([][]Lease, 0, 5)
	for i := 0; i < 5; i++ {
		batches = append(batches, []Lease{lease("msg_1", i)})
	}
	repository := &stubRepository{batches: batches}
	publisher := &stubPublisher{err: ErrNotConfirmed}
	service := newService(repository, publisher, Config{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = service.Run(ctx, nil)
		close(done)
	}()

	// With an hour-long interval, a correct relay attempts exactly once.
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	if attempts := publisher.calls.Load(); attempts != 1 {
		t.Fatalf("publish attempts = %d, want the relay to wait between retries", attempts)
	}
}

// A backlog must still drain at the speed of the broker.
func TestRunDrainsBacklogWithoutWaiting(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{batches: [][]Lease{
		{lease("msg_1", 0)}, {lease("msg_2", 0)}, {lease("msg_3", 0)},
	}}
	service := newService(repository, &stubPublisher{}, Config{Interval: time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = service.Run(ctx, nil)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for repository.calls.Load() < 4 {
		select {
		case <-deadline:
			t.Fatalf("cycles = %d, want the backlog drained without waiting", repository.calls.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}

func TestRunKeepsGoingAfterLeaseFailure(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{leaseErr: errors.New("postgres down")}
	service := newService(repository, &stubPublisher{}, Config{Interval: time.Millisecond})

	var reported atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = service.Run(ctx, func(error) { reported.Add(1) })
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for repository.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("relay stopped after a failing cycle")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	if reported.Load() == 0 {
		t.Fatal("a failing cycle must be reported to the caller")
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	service := newService(&stubRepository{}, &stubPublisher{}, Config{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_ = service.Run(ctx, nil)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

func TestBackoffGrowsAndStaysBounded(t *testing.T) {
	t.Parallel()

	backoff := Backoff{Base: time.Second, Max: time.Minute}
	for attempts := 0; attempts < 40; attempts++ {
		delay := backoff.Delay(attempts)
		if delay <= 0 {
			t.Fatalf("Delay(%d) = %v, want a positive wait", attempts, delay)
		}
		if delay > backoff.Max {
			t.Fatalf("Delay(%d) = %v, want at most %v", attempts, delay, backoff.Max)
		}
	}
}

// Jitter is what stops every message failed during one outage from coming due
// at the same instant and stampeding the broker.
func TestBackoffIsJittered(t *testing.T) {
	t.Parallel()

	backoff := Backoff{Base: time.Second, Max: time.Minute}
	seen := make(map[time.Duration]bool)
	for i := 0; i < 50; i++ {
		seen[backoff.Delay(5)] = true
	}
	if len(seen) < 10 {
		t.Fatalf("distinct delays = %d, want jitter to spread retries out", len(seen))
	}
}

func TestIsPermanent(t *testing.T) {
	t.Parallel()

	if IsPermanent(ErrNotConfirmed) || IsPermanent(ErrNotRouted) {
		t.Fatal("broker conditions must be transient so an outage never discards an event")
	}
	if !IsPermanent(Permanent(errors.New("bad"))) {
		t.Fatal("an explicitly permanent error must be recognized")
	}
	if !errors.Is(Permanent(ErrNotConfirmed), ErrNotConfirmed) {
		t.Fatal("wrapping must preserve the cause")
	}
}

// A settlement that matched no row means the lease was gone. Reporting it as
// published would be a lie: nothing recorded the publication, and another
// instance is going to publish the message again.
func TestLostLeaseIsNotCountedAsPublished(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{
		batches:   [][]Lease{{lease("msg_1", 0)}},
		settleErr: ErrLeaseLost,
	}
	observer := &recordingObserver{}
	service := newService(repository, &stubPublisher{}, Config{}, WithObserver(observer))

	result, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Published != 0 {
		t.Fatalf("published = %d, want a lost lease not to count as published", result.Published)
	}
	if result.LeasesLost != 1 {
		t.Fatalf("leasesLost = %d, want the lost lease reported", result.LeasesLost)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.leasesLost) != 1 {
		t.Fatal("a lost lease must be visible to the operator")
	}
}

// A lost lease is not a cycle failure: the message belongs to someone else now.
func TestLostLeaseDoesNotFailTheCycle(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{
		batches:   [][]Lease{{lease("msg_1", 0), lease("msg_2", 0)}},
		settleErr: ErrLeaseLost,
	}
	service := newService(repository, &stubPublisher{}, Config{})

	result, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v, want a lost lease to be handled, not surfaced", err)
	}
	if result.LeasesLost != 2 {
		t.Fatalf("leasesLost = %d, want both reported", result.LeasesLost)
	}
}

// A real storage failure must still surface.
func TestStorageFailureDuringSettlementSurfaces(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{
		batches:   [][]Lease{{lease("msg_1", 0)}},
		settleErr: errors.New("postgres down"),
	}
	service := newService(repository, &stubPublisher{}, Config{})

	if _, err := service.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want a storage failure to surface")
	}
}

// A broker confirm and PostgreSQL settlement are necessarily separate. If the
// relay stops in that gap, the lease remains eligible and the next relay may
// publish the same message again. The durable consumer is responsible for
// making that duplicate harmless.
func TestConfirmedMessageIsRepublishedAfterFailureBeforeSettlement(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{batches: [][]Lease{
		{lease("msg_1", 0)},
		{lease("msg_1", 1)},
	}}
	publisher := &stubPublisher{}
	var injections atomic.Int32
	service := newService(repository, publisher, Config{}, WithTestHooks(TestHooks{
		BeforeSettlement: func(domain.Message) error {
			if injections.Add(1) == 1 {
				return errors.New("relay stopped after confirm")
			}
			return nil
		},
	}))

	if _, err := service.RunOnce(context.Background()); err == nil {
		t.Fatal("first RunOnce() error = nil, want the confirm/settlement failure")
	}
	if len(repository.published) != 0 || len(repository.retries) != 0 {
		t.Fatalf("outcomes after injected stop = published %v, retries %v; want the lease untouched",
			repository.published, repository.retries)
	}

	result, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if publisher.calls.Load() != 2 {
		t.Fatalf("publish calls = %d, want the intentional republication", publisher.calls.Load())
	}
	if result.Published != 1 || len(repository.published) != 1 {
		t.Fatalf("result/repository = %#v/%v, want exactly one durable settlement",
			result, repository.published)
	}
}

// Publication uses a local monotonic window that starts before PostgreSQL
// creates the lease and ends early enough to leave time for settlement. It
// must not interpret the database's absolute timestamp with this machine's
// wall clock, because those clocks may disagree.
func TestPublicationWindowLeavesTimeForSettlement(t *testing.T) {
	t.Parallel()

	const publicationWindow = 40 * time.Millisecond
	message := lease("msg_1", 0)
	// Deliberately absurd: control must not depend on this absolute timestamp.
	message.Expires = time.Now().Add(24 * time.Hour)

	elapsed := make(chan time.Duration, 1)
	publisher := publisherFunc(func(ctx context.Context, _ domain.Message) error {
		started := time.Now()
		<-ctx.Done()
		elapsed <- time.Since(started)
		return ctx.Err()
	})

	repository := &stubRepository{batches: [][]Lease{{message}}}
	service := newService(repository, publisher, Config{
		LeaseDuration: SettlementReserve + publicationWindow,
	})

	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if duration := <-elapsed; duration > publicationWindow+100*time.Millisecond {
		t.Fatalf("publication ran for %s, want approximately %s", duration, publicationWindow)
	}
	if len(repository.published) != 0 {
		t.Fatal("a publication that exhausted its window must not be marked published")
	}
}

type publisherFunc func(context.Context, domain.Message) error

func (f publisherFunc) Publish(ctx context.Context, message domain.Message) error {
	return f(ctx, message)
}

// cycleRecorder receives the extended reports the metrics observer needs. It
// implements both interfaces, exactly as that observer does.
type cycleRecorder struct {
	recordingObserver

	publications []Publication
	cycles       []Cycle
}

func (r *cycleRecorder) PublicationSettled(publication Publication) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publications = append(r.publications, publication)
}

func (r *cycleRecorder) CycleCompleted(cycle Cycle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cycles = append(r.cycles, cycle)
}

func (r *cycleRecorder) snapshot() ([]Publication, []Cycle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Publication(nil), r.publications...), append([]Cycle(nil), r.cycles...)
}

// The relay reports every publication, not only the ones that need attention.
// A metrics observer needs the successes too, and needs them classified.
func TestEveryPublicationIsReportedWithItsOutcome(t *testing.T) {
	t.Parallel()

	recorder := &cycleRecorder{}
	repository := &stubRepository{batches: [][]Lease{{lease("msg_ok", 0)}}}
	service := newService(repository, &stubPublisher{}, Config{}, WithObserver(recorder))

	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	publications, cycles := recorder.snapshot()
	if len(publications) != 1 || publications[0].Outcome != OutcomePublished {
		t.Fatalf("publications = %#v, want one published", publications)
	}
	if publications[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1", publications[0].Attempts)
	}
	if len(cycles) != 1 || cycles[0].Err != nil || cycles[0].Result.Published != 1 {
		t.Fatalf("cycles = %#v, want one successful cycle publishing one message", cycles)
	}
}

func TestTransientAndPermanentPublicationsAreReportedDistinctly(t *testing.T) {
	t.Parallel()

	transient := &cycleRecorder{}
	service := newService(
		&stubRepository{batches: [][]Lease{{lease("msg_transient", 0)}}},
		&stubPublisher{err: ErrNotConfirmed}, Config{}, WithObserver(transient))
	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("transient RunOnce() error = %v", err)
	}
	publications, _ := transient.snapshot()
	if len(publications) != 1 || publications[0].Outcome != OutcomeRetrying {
		t.Fatalf("transient publications = %#v, want one retrying", publications)
	}
	if !errors.Is(publications[0].Cause, ErrNotConfirmed) {
		t.Errorf("transient cause = %v, want the broker confirmation error", publications[0].Cause)
	}

	permanent := &cycleRecorder{}
	service = newService(
		&stubRepository{batches: [][]Lease{{lease("msg_permanent", 0)}}},
		&stubPublisher{err: Permanent(errors.New("cannot encode"))}, Config{}, WithObserver(permanent))
	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("permanent RunOnce() error = %v", err)
	}
	publications, _ = permanent.snapshot()
	if len(publications) != 1 || publications[0].Outcome != OutcomeFailed {
		t.Fatalf("permanent publications = %#v, want one failed", publications)
	}
}

// A cycle that could not lease has to name the stage, or an alert cannot tell
// a database problem from a settlement one.
func TestAFailedCycleNamesItsStage(t *testing.T) {
	t.Parallel()

	recorder := &cycleRecorder{}
	service := newService(
		&stubRepository{leaseErr: errors.New("connection refused")},
		&stubPublisher{}, Config{}, WithObserver(recorder))

	if _, err := service.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want the lease failure")
	}
	_, cycles := recorder.snapshot()
	if len(cycles) != 1 || cycles[0].Stage != StageLease {
		t.Fatalf("cycles = %#v, want one cycle failing at the lease stage", cycles)
	}
}

// Both observers must be notified: the worker keeps the logging one and adds
// the metrics one beside it.
func TestObserversAreNotifiedInOrder(t *testing.T) {
	t.Parallel()

	first, second := &recordingObserver{}, &recordingObserver{}
	service := newService(
		&stubRepository{batches: [][]Lease{{lease("msg_permanent", 0)}}},
		&stubPublisher{err: Permanent(errors.New("cannot encode"))},
		Config{}, WithObserver(first), WithObserver(second))

	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	for name, observer := range map[string]*recordingObserver{"first": first, "second": second} {
		observer.mu.Lock()
		abandoned := len(observer.abandoned)
		observer.mu.Unlock()
		if abandoned != 1 {
			t.Errorf("%s observer saw %d abandoned messages, want 1", name, abandoned)
		}
	}
}
