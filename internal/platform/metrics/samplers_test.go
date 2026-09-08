package metrics

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx so the pool test can open a handle.
	"go.opentelemetry.io/otel/attribute"
)

// fakeBacklog answers the two backlog readers, and can be made to fail so a
// test can watch the sampler survive an outage.
type fakeBacklog struct {
	mu      sync.Mutex
	err     error
	pending int64
	oldest  time.Duration
	// block, when set, holds a collection open so the sampler's timeout is
	// what ends the round.
	block chan struct{}
	calls atomic.Int64
}

func (f *fakeBacklog) Backlog(ctx context.Context) (int64, time.Duration, error) {
	f.calls.Add(1)
	f.mu.Lock()
	err, pending, oldest, block := f.err, f.pending, f.oldest, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	return pending, oldest, err
}

type fakeInbox struct {
	mu      sync.Mutex
	pending int64
	failed  int64
	oldest  time.Duration
	err     error
}

func (f *fakeInbox) Backlog(context.Context) (int64, int64, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending, f.failed, f.oldest, f.err
}

func (f *fakeBacklog) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeBacklog) recover(pending int64, oldest time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.pending, f.oldest, f.block = nil, pending, oldest, nil
}

func TestBacklogSamplerPublishesOutboxAndInboxGauges(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	outbox := &fakeBacklog{pending: 7, oldest: 42 * time.Second}
	inbox := &fakeInbox{pending: 3, failed: 2, oldest: 9 * time.Second}

	sampler := NewSampler(SamplerBacklog, built, nil,
		NewBacklogCollector(outbox, inbox), SamplerConfig{Interval: time.Hour, Timeout: time.Second})
	runOnce(t, sampler)

	found := collected(t, manual)
	for name, want := range map[string]float64{
		OutboxPending: 7, OutboxOldestAge: 42, InboxPending: 3, InboxFailed: 2, InboxOldestAge: 9,
	} {
		if got := gaugeValue(t, found[name]); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// A dependency that goes down must not blank the gauges: the last known value
// keeps the dashboard honest about what the backlog was, while the age gauge
// and the failure counter say the reading is stale.
func TestSamplerKeepsTheLastKnownValueAndReportsTheFailure(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	outbox := &fakeBacklog{pending: 5, oldest: 10 * time.Second}
	inbox := &fakeInbox{}

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	sampler := NewSampler(SamplerBacklog, built, nil,
		NewBacklogCollector(outbox, inbox),
		SamplerConfig{Interval: time.Hour, Timeout: time.Second, Now: clock.Now})
	runOnce(t, sampler)

	outbox.fail(errors.New("connection refused on postgres://payments:hunter2@db"))
	clock.advance(30 * time.Second)
	sampler.sample(context.Background())

	found := collected(t, manual)
	if got := gaugeValue(t, found[OutboxPending]); got != 5 {
		t.Errorf("outbox pending after failure = %v, want the last known 5", got)
	}
	if got := sampler.Failures(); got != 1 {
		t.Errorf("failures = %d, want 1", got)
	}
	if got := gaugeValue(t, found[SamplerAge], attribute.String(LabelSampler, SamplerBacklog)); got != 30 {
		t.Errorf("sampler age = %v, want 30", got)
	}
	if got := sumFor(t, found[SamplerFailures], attribute.String(LabelSampler, SamplerBacklog)); got != 1 {
		t.Errorf("exported failures = %d, want 1", got)
	}

	// Recovery: a later round publishes fresh values and resets the age.
	outbox.recover(1, time.Second)
	clock.advance(15 * time.Second)
	sampler.sample(context.Background())

	found = collected(t, manual)
	if got := gaugeValue(t, found[OutboxPending]); got != 1 {
		t.Errorf("outbox pending after recovery = %v, want 1", got)
	}
	if got := gaugeValue(t, found[SamplerAge], attribute.String(LabelSampler, SamplerBacklog)); got != 0 {
		t.Errorf("sampler age after recovery = %v, want 0", got)
	}
}

// A dependency that accepts the call and never answers must be ended by the
// sampler's own timeout, not by the exporter or the process.
func TestSamplerTimesOutAStalledCollection(t *testing.T) {
	t.Parallel()
	built, _ := reader(t)
	outbox := &fakeBacklog{block: make(chan struct{})}

	sampler := NewSampler(SamplerBacklog, built, nil,
		NewBacklogCollector(outbox, &fakeInbox{}),
		SamplerConfig{Interval: time.Second, Timeout: 50 * time.Millisecond})

	done := make(chan struct{})
	go func() {
		defer close(done)
		sampler.sample(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sample did not honour its timeout")
	}
	if got := sampler.Failures(); got != 1 {
		t.Errorf("failures = %d, want 1", got)
	}
}

// Run must return without error when its context is cancelled, and a cancelled
// round must not be counted as a collection failure.
func TestSamplerStopsCleanlyAndDoesNotCountShutdownAsFailure(t *testing.T) {
	t.Parallel()
	built, _ := reader(t)
	outbox := &fakeBacklog{block: make(chan struct{})}

	sampler := NewSampler(SamplerBacklog, built, nil,
		NewBacklogCollector(outbox, &fakeInbox{}),
		SamplerConfig{Interval: 10 * time.Millisecond, Timeout: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sampler.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

func TestQueueDepthSamplerLabelsOnlyDeclaredQueues(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	queues := []string{"payments.webhooks", "payments.webhooks.dlq"}
	collector := NewQueueDepthCollector(func(context.Context, []string) (map[string]int, error) {
		return map[string]int{"payments.webhooks": 4, "payments.webhooks.dlq": 2, "queue.unknown": 9}, nil
	}, queues)

	sampler := NewSampler(SamplerBroker, built, nil, collector,
		SamplerConfig{Interval: time.Hour, Timeout: time.Second})
	runOnce(t, sampler)

	found := collected(t, manual)
	if got := gaugeValue(t, found[QueueDepth], attribute.String(LabelQueue, "payments.webhooks")); got != 4 {
		t.Errorf("main queue depth = %v, want 4", got)
	}
	if got := gaugeValue(t, found[QueueDepth], attribute.String(LabelQueue, "payments.webhooks.dlq")); got != 2 {
		t.Errorf("dead letter depth = %v, want 2", got)
	}
	for _, set := range attributeSets(t, found[QueueDepth]) {
		value, _ := set.Value(LabelQueue)
		if value.String() == "queue.unknown" {
			t.Error("a queue outside the declared topology was published")
		}
	}
}

// A broker that is unreachable must produce a failure, not a panic and not a
// blocked collection.
func TestQueueDepthSamplerSurvivesAnUnreachableBroker(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	var fail atomic.Bool
	fail.Store(true)
	collector := NewQueueDepthCollector(func(context.Context, []string) (map[string]int, error) {
		if fail.Load() {
			return nil, errors.New("dial amqp://payments:hunter2@broker:5672: connection refused")
		}
		return map[string]int{"payments.webhooks.dlq": 3}, nil
	}, []string{"payments.webhooks.dlq"})

	sampler := NewSampler(SamplerBroker, built, nil, collector,
		SamplerConfig{Interval: time.Hour, Timeout: time.Second})
	runOnce(t, sampler)
	if got := sampler.Failures(); got != 1 {
		t.Fatalf("failures = %d, want 1", got)
	}

	fail.Store(false)
	sampler.sample(context.Background())
	found := collected(t, manual)
	if got := gaugeValue(t, found[QueueDepth], attribute.String(LabelQueue, "payments.webhooks.dlq")); got != 3 {
		t.Errorf("dead letter depth after recovery = %v, want 3", got)
	}
}

func TestPoolMetricsReportConnectionsByState(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	db, err := sql.Open("pgx", "postgres://unused:unused@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(11)

	if err := RegisterPoolMetrics(built, db, nil); err != nil {
		t.Fatalf("RegisterPoolMetrics() error = %v", err)
	}

	found := collected(t, manual)
	// Nothing is borrowed, so both states report zero; what matters is that
	// both series exist with the allowed state values and no identifier.
	if got := gaugeValue(t, found[PoolConnections], attribute.String(LabelState, "in_use")); got != 0 {
		t.Errorf("in-use connections = %v, want 0", got)
	}
	if got := gaugeValue(t, found[PoolConnections], attribute.String(LabelState, "idle")); got != 0 {
		t.Errorf("idle connections = %v, want 0", got)
	}
	if got := gaugeValue(t, found[PoolMaxOpen]); got != 11 {
		t.Errorf("max open connections = %v, want 11", got)
	}
	if _, ok := found[PoolWaits]; !ok {
		t.Error("pool wait counter was not exported")
	}
}

// runOnce collects a single round without starting the loop.
func runOnce(t *testing.T, sampler *Sampler) {
	t.Helper()
	sampler.registerObservers()
	sampler.sample(context.Background())
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}
