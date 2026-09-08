package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	app "github.com/rmotti/payments-boilerplate/internal/application/consumer"
)

// handlerFunc adapts a function to the Handler port.
type handlerFunc func(ctx context.Context, eventID string) (app.Handling, error)

func (f handlerFunc) Handle(ctx context.Context, eventID string) (app.Handling, error) {
	return f(ctx, eventID)
}

type recordingAcknowledger struct {
	mu       sync.Mutex
	acks     int
	nacks    int
	rejects  int
	onAction func(string)
}

func (a *recordingAcknowledger) Ack(uint64, bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acks++
	if a.onAction != nil {
		a.onAction("ack")
	}
	return nil
}

func (a *recordingAcknowledger) Nack(uint64, bool, bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nacks++
	return nil
}

func (a *recordingAcknowledger) Reject(uint64, bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rejects++
	return nil
}

func (a *recordingAcknowledger) counts() (acks, nacks, rejects int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acks, a.nacks, a.rejects
}

func testDelivery(acknowledger amqp.Acknowledger) amqp.Delivery {
	return amqp.Delivery{
		Acknowledger: acknowledger,
		DeliveryTag:  1,
		MessageId:    "msg_test",
		RoutingKey:   "payment.webhook.checkout.completed",
		Body:         []byte(`{"webhookEventId":"evt_test"}`),
	}
}

// recordingObserver collects what the consumer did, for assertions.
type recordingObserver struct {
	mu           sync.Mutex
	republished  []string
	rejected     int
	failed       int
	failureCause error
}

func (o *recordingObserver) Rejected(string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rejected++
}

func (o *recordingObserver) Republished(_, destination string, _ int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.republished = append(o.republished, destination)
}

func (o *recordingObserver) Failed(_ string, cause error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed++
	o.failureCause = cause
}

func (o *recordingObserver) counts() (republished []string, rejected, failed int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.republished...), o.rejected, o.failed
}

// testTiers keeps delays short enough for a test to wait on them.
func testTiers() []RetryTier {
	return []RetryTier{{Delay: 2 * time.Second}, {Delay: 4 * time.Second}}
}

// prepareBroker declares the topology and empties every queue the test uses,
// so leftovers from another run cannot be mistaken for this run's messages.
func prepareBroker(t *testing.T, connection *Connection, tiers []RetryTier) *amqp.Channel {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	channel, err := connection.Channel(ctx)
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })

	if err := DeclareTopology(channel); err != nil {
		t.Fatalf("declare topology: %v", err)
	}
	if err := DeclareRetryTopology(channel, tiers); err != nil {
		t.Fatalf("declare retry topology: %v", err)
	}
	for _, queue := range append([]string{WebhooksQueue, DeadLetterQueue}, tierQueues(tiers)...) {
		if _, err := channel.QueuePurge(queue, false); err != nil {
			t.Fatalf("purge %s: %v", queue, err)
		}
	}
	return channel
}

func tierQueues(tiers []RetryTier) []string {
	queues := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		queues = append(queues, tier.Queue())
	}
	return queues
}

// publishReference puts one message on the main queue, shaped the way the
// relay publishes: a reference to a stored event, never a payload.
func publishReference(t *testing.T, channel *amqp.Channel, eventID string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body := fmt.Sprintf(`{"messageId":"msg_%s","type":"checkout.completed",`+
		`"schemaVersion":1,"webhookEventId":%q}`, eventID, eventID)
	if err := channel.PublishWithContext(ctx, EventsExchange,
		"payment.webhook.checkout.completed", false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    "msg_" + eventID,
			Body:         []byte(body),
		}); err != nil {
		t.Fatalf("publish reference: %v", err)
	}
}

func queueDepth(t *testing.T, connection *Connection, queue string) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	channel, err := connection.Channel(ctx)
	if err != nil {
		t.Fatalf("open inspect channel: %v", err)
	}
	defer func() { _ = channel.Close() }()

	// Passive declare reports the depth without changing anything.
	state, err := channel.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatalf("inspect %s: %v", queue, err)
	}
	return state.Messages
}

// waitFor polls until condition holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startConsumer(t *testing.T, consumer *Consumer) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = consumer.Run(ctx, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("consumer did not stop within the shutdown budget")
		}
	})
}

func TestConsumerAcknowledgesOnlyAfterCommittedHandling(t *testing.T) {
	t.Parallel()

	var order []string
	acknowledger := &recordingAcknowledger{onAction: func(action string) {
		order = append(order, action)
	}}
	consumer := NewConsumer(nil, nil, handlerFunc(func(context.Context, string) (app.Handling, error) {
		order = append(order, "commit")
		return app.Handling{Disposition: app.DispositionDone, Applied: true}, nil
	}), ConsumerConfig{})
	consumer.WithTestHooks(TestHooks{BeforeAck: func(amqp.Delivery) error {
		order = append(order, "before-ack")
		return nil
	}})

	if err := consumer.handle(context.Background(), testDelivery(acknowledger)); err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	if got := fmt.Sprint(order); got != "[commit before-ack ack]" {
		t.Fatalf("order = %s, want commit before acknowledgement", got)
	}
}

func TestConsumerConfirmsRetryCopyBeforeAcknowledgingOriginal(t *testing.T) {
	t.Parallel()

	var order []string
	acknowledger := &recordingAcknowledger{onAction: func(action string) {
		order = append(order, action)
	}}
	tier := RetryTier{Delay: time.Second}
	consumer := NewConsumer(nil, nil, handlerFunc(func(context.Context, string) (app.Handling, error) {
		return app.Handling{Disposition: app.DispositionRetry, Attempts: 1}, nil
	}), ConsumerConfig{RetryTiers: []RetryTier{tier}})
	consumer.WithTestHooks(TestHooks{
		Republish: func(_ context.Context, exchange string, _ amqp.Delivery) error {
			if exchange != tier.Exchange() {
				t.Fatalf("exchange = %q, want %q", exchange, tier.Exchange())
			}
			order = append(order, "publish")
			return nil
		},
		AfterRepublishConfirmed: func(string, amqp.Delivery) error {
			order = append(order, "confirm")
			return nil
		},
		BeforeAck: func(amqp.Delivery) error {
			order = append(order, "before-ack")
			return nil
		},
	})

	if err := consumer.handle(context.Background(), testDelivery(acknowledger)); err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	if got := fmt.Sprint(order); got != "[publish confirm before-ack ack]" {
		t.Fatalf("order = %s, want confirmed copy before acknowledgement", got)
	}
}

func TestConsumerLeavesOriginalUnacknowledgedWhenRepublishFails(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("retry exchange unavailable")
	acknowledger := &recordingAcknowledger{}
	consumer := NewConsumer(nil, nil, handlerFunc(func(context.Context, string) (app.Handling, error) {
		return app.Handling{Disposition: app.DispositionRetry, Attempts: 1}, nil
	}), ConsumerConfig{RetryTiers: []RetryTier{{Delay: time.Second}}})
	consumer.WithTestHooks(TestHooks{
		Republish: func(context.Context, string, amqp.Delivery) error { return wantErr },
	})

	err := consumer.handle(context.Background(), testDelivery(acknowledger))
	if !errors.Is(err, wantErr) {
		t.Fatalf("handle() error = %v, want %v", err, wantErr)
	}
	if acks, nacks, rejects := acknowledger.counts(); acks != 0 || nacks != 0 || rejects != 0 {
		t.Fatalf("settlements = ack %d/nack %d/reject %d, want original untouched",
			acks, nacks, rejects)
	}
}

func TestConsumerLeavesOriginalUnacknowledgedAfterConfirmedCopyIfAckWindowFails(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("worker stopped after republish confirm")
	acknowledger := &recordingAcknowledger{}
	consumer := NewConsumer(nil, nil, handlerFunc(func(context.Context, string) (app.Handling, error) {
		return app.Handling{Disposition: app.DispositionDead, Attempts: 10}, nil
	}), ConsumerConfig{})
	consumer.WithTestHooks(TestHooks{
		Republish: func(context.Context, string, amqp.Delivery) error { return nil },
		AfterRepublishConfirmed: func(exchange string, _ amqp.Delivery) error {
			if exchange != DeadLetterExchange {
				t.Fatalf("exchange = %q, want dead-letter exchange", exchange)
			}
			return wantErr
		},
	})

	err := consumer.handle(context.Background(), testDelivery(acknowledger))
	if !errors.Is(err, wantErr) {
		t.Fatalf("handle() error = %v, want %v", err, wantErr)
	}
	if acks, _, _ := acknowledger.counts(); acks != 0 {
		t.Fatalf("acks = %d, want the confirmed copy to coexist with an unacknowledged original", acks)
	}
}

func TestConsumerAcknowledgesAppliedMessages(t *testing.T) {
	connection := openTestBroker(t)
	publisher := openTestBroker(t)
	tiers := testTiers()
	channel := prepareBroker(t, connection, tiers)

	var handled atomic.Int64
	consumer := NewConsumer(connection, publisher,
		handlerFunc(func(_ context.Context, _ string) (app.Handling, error) {
			handled.Add(1)
			return app.Handling{Disposition: app.DispositionDone, Applied: true}, nil
		}),
		ConsumerConfig{Prefetch: 4, Concurrency: 2, RetryTiers: tiers})
	startConsumer(t, consumer)

	publishReference(t, channel, "evt_done")

	waitFor(t, 15*time.Second, "the message to be handled", func() bool {
		return handled.Load() == 1
	})
	waitFor(t, 15*time.Second, "the main queue to drain", func() bool {
		return queueDepth(t, connection, WebhooksQueue) == 0
	})
}

// A transient failure must reach a retry tier and come back, not spin.
func TestConsumerSendsTransientFailuresThroughARetryTier(t *testing.T) {
	connection := openTestBroker(t)
	publisher := openTestBroker(t)
	tiers := testTiers()
	channel := prepareBroker(t, connection, tiers)

	var attempts atomic.Int64
	observer := &recordingObserver{}
	consumer := NewConsumer(connection, publisher,
		handlerFunc(func(_ context.Context, _ string) (app.Handling, error) {
			// Fail once, then succeed, so the message must survive a full
			// round trip through a tier to be handled a second time.
			if attempts.Add(1) == 1 {
				return app.Handling{
					Disposition: app.DispositionRetry,
					Attempts:    1,
					Cause:       errors.New("database unavailable"),
				}, nil
			}
			return app.Handling{Disposition: app.DispositionDone, Applied: true}, nil
		}),
		ConsumerConfig{Prefetch: 4, Concurrency: 2, RetryTiers: tiers})
	consumer.WithObserver(observer)
	startConsumer(t, consumer)

	publishReference(t, channel, "evt_retry")

	waitFor(t, 30*time.Second, "the message to be retried and then applied", func() bool {
		return attempts.Load() >= 2
	})

	republished, _, _ := observer.counts()
	if len(republished) == 0 || republished[0] != tiers[0].Queue() {
		t.Errorf("republished = %v, want the first tier %s", republished, tiers[0].Queue())
	}
	waitFor(t, 15*time.Second, "every queue to drain", func() bool {
		return queueDepth(t, connection, WebhooksQueue) == 0 &&
			queueDepth(t, connection, tiers[0].Queue()) == 0
	})
}

// A terminal failure must land in the dead-letter queue, published explicitly
// and confirmed rather than left to the classic queue's unconfirmed
// dead-lettering.
func TestConsumerSendsTerminalFailuresToTheDeadLetterQueue(t *testing.T) {
	connection := openTestBroker(t)
	publisher := openTestBroker(t)
	tiers := testTiers()
	channel := prepareBroker(t, connection, tiers)

	observer := &recordingObserver{}
	consumer := NewConsumer(connection, publisher,
		handlerFunc(func(_ context.Context, _ string) (app.Handling, error) {
			return app.Handling{
				Disposition: app.DispositionDead,
				Attempts:    10,
				Cause:       errors.New("amount does not match"),
			}, nil
		}),
		ConsumerConfig{Prefetch: 4, Concurrency: 2, RetryTiers: tiers})
	consumer.WithObserver(observer)
	startConsumer(t, consumer)

	publishReference(t, channel, "evt_dead")

	waitFor(t, 20*time.Second, "the message to reach the dead-letter queue", func() bool {
		return queueDepth(t, connection, DeadLetterQueue) == 1
	})
	waitFor(t, 15*time.Second, "the main queue to drain", func() bool {
		return queueDepth(t, connection, WebhooksQueue) == 0
	})

	republished, _, _ := observer.counts()
	if len(republished) != 1 || republished[0] != DeadLetterQueue {
		t.Errorf("republished = %v, want exactly the dead-letter queue", republished)
	}
}

// A message the consumer cannot even read has no inbox row to record against,
// so it goes straight to the dead letter rather than being retried forever.
func TestConsumerRejectsUnreadableMessages(t *testing.T) {
	connection := openTestBroker(t)
	publisher := openTestBroker(t)
	tiers := testTiers()
	channel := prepareBroker(t, connection, tiers)

	observer := &recordingObserver{}
	consumer := NewConsumer(connection, publisher,
		handlerFunc(func(_ context.Context, _ string) (app.Handling, error) {
			t.Error("an unreadable message must never reach the handler")
			return app.Handling{}, nil
		}),
		ConsumerConfig{Prefetch: 4, Concurrency: 1, RetryTiers: tiers})
	consumer.WithObserver(observer)
	startConsumer(t, consumer)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := channel.PublishWithContext(ctx, EventsExchange,
		"payment.webhook.checkout.completed", false, false, amqp.Publishing{
			ContentType: "application/json", DeliveryMode: amqp.Persistent,
			MessageId: "msg_broken", Body: []byte(`{"messageId":"msg_broken"}`),
		}); err != nil {
		t.Fatalf("publish broken message: %v", err)
	}

	waitFor(t, 20*time.Second, "the unreadable message to be dead-lettered", func() bool {
		return queueDepth(t, connection, DeadLetterQueue) == 1
	})
	republished, rejected, _ := observer.counts()
	if rejected != 1 {
		t.Errorf("rejected = %d, want 1", rejected)
	}
	if len(republished) != 1 || republished[0] != DeadLetterQueue {
		t.Errorf("republished = %v, want an explicitly confirmed dead-letter", republished)
	}
}

// If applying the message could not even be recorded, the message must stay
// unacknowledged so the broker redelivers it. Acknowledging on an unknown
// outcome would drop a financial event.
func TestConsumerLeavesUnrecordedFailuresUnacknowledged(t *testing.T) {
	connection := openTestBroker(t)
	publisher := openTestBroker(t)
	tiers := testTiers()
	channel := prepareBroker(t, connection, tiers)

	observer := &recordingObserver{}
	var seen atomic.Int64
	consumer := NewConsumer(connection, publisher,
		handlerFunc(func(_ context.Context, _ string) (app.Handling, error) {
			seen.Add(1)
			return app.Handling{}, errors.New("could not record the failure")
		}),
		ConsumerConfig{Prefetch: 4, Concurrency: 1, RetryTiers: tiers})
	consumer.WithObserver(observer)
	startConsumer(t, consumer)

	publishReference(t, channel, "evt_unrecorded")

	waitFor(t, 20*time.Second, "the unacknowledged message to be redelivered", func() bool {
		_, _, failed := observer.counts()
		return seen.Load() >= 2 && failed >= 2
	})

	// Nothing may have been acknowledged, retried or dead-lettered. Closing the
	// failed session is what releases the unacknowledged delivery; the reconnect
	// backoff prevents a tight loop while preserving eventual progress.
	republished, _, _ := observer.counts()
	if len(republished) != 0 {
		t.Errorf("republished = %v, want nothing republished", republished)
	}
	if depth := queueDepth(t, connection, DeadLetterQueue); depth != 0 {
		t.Errorf("dead-letter depth = %d, want 0", depth)
	}
}

func TestTierForWalksTheLadderAndHoldsAtTheLast(t *testing.T) {
	t.Parallel()

	tiers := []RetryTier{{Delay: time.Second}, {Delay: time.Minute}, {Delay: time.Hour}}
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: time.Second},
		{attempts: 1, want: time.Second},
		{attempts: 2, want: time.Minute},
		{attempts: 3, want: time.Hour},
		{attempts: 9, want: time.Hour},
	}
	for _, test := range tests {
		got, ok := TierFor(tiers, test.attempts)
		if !ok || got.Delay != test.want {
			t.Errorf("TierFor(%d) = %s, %t, want %s", test.attempts, got.Delay, ok, test.want)
		}
	}

	if _, ok := TierFor(nil, 1); ok {
		t.Error("TierFor(nil) reported a tier, want none")
	}
}

// deliveryRecorder implements both consumer observer interfaces, exactly as
// the metrics observer does, so the extension is exercised the way production
// wires it.
type deliveryRecorder struct {
	recordingObserver

	redelivered atomic.Int32
}

func (r *deliveryRecorder) Redelivered(string) { r.redelivered.Add(1) }

// The worker installs the logging observer and the metrics one side by side,
// so both have to receive every event.
func TestConsumerNotifiesEveryRegisteredObserver(t *testing.T) {
	t.Parallel()

	first, second := &recordingObserver{}, &recordingObserver{}
	consumer := NewConsumer(nil, nil, nil, ConsumerConfig{}).
		WithObserver(first).
		WithObserver(second)

	if err := consumer.failed("msg_1", errors.New("unrecorded")); err == nil {
		t.Fatal("failed() error = nil, want the cause returned unchanged")
	}

	for name, observer := range map[string]*recordingObserver{"first": first, "second": second} {
		if _, _, failed := observer.counts(); failed != 1 {
			t.Errorf("%s observer saw %d failures, want 1", name, failed)
		}
	}
}

// A redelivery is the broker telling us a message came back. It has to reach
// the observer before the message is handled, and only when the flag is set.
func TestConsumerReportsRedeliveredMessages(t *testing.T) {
	t.Parallel()

	recorder := &deliveryRecorder{}
	// The settlement paths need a live channel, so the ack window is what
	// ends each call here. What is under test is the observer notification,
	// which happens before any of that.
	settlementFailure := errors.New("no channel in this test")
	consumer := NewConsumer(nil, nil, nil, ConsumerConfig{}).
		WithObserver(recorder).
		WithTestHooks(TestHooks{
			Republish: func(context.Context, string, amqp.Delivery) error { return settlementFailure },
		})

	ctx := context.Background()
	// An unreadable body takes the dead-letter path without reaching the
	// handler, which is what lets this exercise the delivery observer alone.
	if err := consumer.handle(ctx, amqp.Delivery{MessageId: "msg_first", Body: []byte("{")}); err == nil {
		t.Fatal("handle() error = nil, want the injected settlement failure")
	}
	if got := recorder.redelivered.Load(); got != 0 {
		t.Fatalf("redeliveries after a first delivery = %d, want 0", got)
	}

	if err := consumer.handle(ctx, amqp.Delivery{
		MessageId: "msg_first", Body: []byte("{"), Redelivered: true,
	}); err == nil {
		t.Fatal("handle() redelivery error = nil, want the injected settlement failure")
	}
	if got := recorder.redelivered.Load(); got != 1 {
		t.Fatalf("redeliveries = %d, want 1", got)
	}
}
