//go:build chaos

package chaos

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	postgresrepo "github.com/rmotti/payments-boilerplate/internal/adapters/postgres/repositories"
	rabbit "github.com/rmotti/payments-boilerplate/internal/adapters/rabbitmq"
	consumerapp "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	outboxapp "github.com/rmotti/payments-boilerplate/internal/application/outbox"
	webhookdomain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"

	amqp "github.com/rabbitmq/amqp091-go"
)

const chaosAllowedEnv = "CHAOS_DESTRUCTIVE_ALLOWED"

func TestLongPostgresOutageLeavesConsumerUnacknowledgedAndRecovers(t *testing.T) {
	databaseURL := requireLocalChaosTarget(t, "TEST_DATABASE_URL")
	target := targetAddress(t, databaseURL, "5432")
	proxy, err := NewTCPProxy(context.Background(), target)
	if err != nil {
		t.Fatalf("NewTCPProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	db, err := sql.Open("pgx", replaceURLHost(t, databaseURL, proxy.Addr()))
	if err != nil {
		t.Fatalf("open proxied PostgreSQL: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		cancelPing()
		t.Fatalf("initial PostgreSQL ping: %v", err)
	}
	cancelPing()

	service := consumerapp.NewService(postgresrepo.NewEventRepository(db), inertInterpreter{}, consumerapp.Config{})
	proxy.Silence()
	proxy.CloseExisting()

	// Both the processing transaction and the short failure-recording
	// transaction hit a real transport outage. Handle must return an error,
	// which is the adapter contract for leaving the delivery unacknowledged.
	handleCtx, cancelHandle := context.WithTimeout(context.Background(), 750*time.Millisecond)
	_, handleErr := service.Handle(handleCtx, "evt_chaos_missing")
	cancelHandle()
	if handleErr == nil {
		t.Fatal("Handle() error = nil while PostgreSQL is silent; delivery could be acknowledged")
	}

	// Wake the accepted silent connection into the reject path, then verify
	// new connections are refused as a distinct failure mode.
	proxy.RejectNew()
	rejectCtx, cancelReject := context.WithTimeout(context.Background(), time.Second)
	if err := db.PingContext(rejectCtx); err == nil {
		cancelReject()
		t.Fatal("PostgreSQL ping succeeded while new connections were rejected")
	}
	cancelReject()

	proxy.Restore()
	waitWithBackoff(t, 10*time.Second, "PostgreSQL pool recovery", func(ctx context.Context) error {
		return db.PingContext(ctx)
	})
}

func TestLongRabbitMQConnectionRecoveryAndBacklogMatrix(t *testing.T) {
	brokerURL := requireLocalChaosTarget(t, "TEST_RABBITMQ_URL")
	direct := openRabbit(t, brokerURL, "chaos-direct")
	channel := prepareDedicatedRabbit(t, direct)

	proxy, err := NewTCPProxy(context.Background(), targetAddress(t, brokerURL, "5672"))
	if err != nil {
		t.Fatalf("NewTCPProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	proxiedURL := replaceURLHost(t, brokerURL, proxy.Addr())

	var handled atomic.Int64
	consumer := rabbit.NewConsumer(
		rabbit.New(proxiedURL, "chaos-consumer"),
		rabbit.New(brokerURL, "chaos-republisher"),
		handlerFunc(func(context.Context, string) (consumerapp.Handling, error) {
			handled.Add(1)
			return consumerapp.Handling{Disposition: consumerapp.DispositionDone, Applied: true}, nil
		}),
		rabbit.ConsumerConfig{Prefetch: 4, Concurrency: 2, RetryTiers: []rabbit.RetryTier{{Delay: time.Second}}},
	)
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	errorsSeen := make(chan error, 16)
	runStopped := false
	go func() { runDone <- consumer.Run(runCtx, func(err error) { errorsSeen <- err }) }()
	t.Cleanup(func() {
		if runStopped {
			return
		}
		cancelRun()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("consumer shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("consumer did not stop")
		}
	})

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
	if err := proxy.WaitForConnections(waitCtx, 2); err != nil {
		cancelWait()
		t.Fatal(err)
	}
	cancelWait()
	publishReferences(t, channel, 0, 1)
	waitCount(t, 15*time.Second, "initial delivery", &handled, 1)

	// Accept the reconnect but withhold the AMQP handshake. This is distinct
	// from a refused port and exercises the bounded setup path.
	proxy.Silence()
	proxy.CloseExisting()
	waitForConsumerError(t, errorsSeen)
	publishReferences(t, channel, 1, 1)
	waitQueueDepth(t, direct, rabbit.WebhooksQueue, 1)
	waitCtx, cancelWait = context.WithTimeout(context.Background(), 10*time.Second)
	if err := proxy.WaitForConnections(waitCtx, 1); err != nil {
		cancelWait()
		t.Fatal(err)
	}
	cancelWait()
	proxy.Restore()
	waitCount(t, 20*time.Second, "delivery after silent recovery", &handled, 2)

	// Refuse new connections after dropping the live channel. Run reports the
	// failed sessions and applies its reconnect backoff until Restore.
	proxy.RejectNew()
	proxy.CloseExisting()
	waitForConsumerError(t, errorsSeen)
	publishReferences(t, channel, 2, 1)
	waitQueueDepth(t, direct, rabbit.WebhooksQueue, 1)
	// Run waits at least its minimum backoff before opening another session;
	// this second error proves a newly attempted connection was actually refused.
	waitForConsumerError(t, errorsSeen)
	proxy.Restore()
	waitCount(t, 20*time.Second, "delivery after refused-connection recovery", &handled, 3)

	cancelRun()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("consumer shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("consumer did not stop")
	}
	runStopped = true

	// Backlog is accumulated while no worker is running, then drained under
	// different concurrency/prefetch settings. Delivery count is allowed to be
	// greater than volume because recovery is at-least-once.
	for index, test := range []struct {
		concurrency int
		prefetch    int
		volume      int
	}{
		{concurrency: 1, prefetch: 1, volume: 40},
		{concurrency: 4, prefetch: 16, volume: 120},
	} {
		publishReferences(t, channel, 1000*(index+1), test.volume)
		waitQueueDepth(t, direct, rabbit.WebhooksQueue, test.volume)

		var matrixHandled atomic.Int64
		var latenciesMu sync.Mutex
		latencies := make([]time.Duration, 0, test.volume)
		started := time.Now()
		matrixCtx, cancelMatrix := context.WithCancel(context.Background())
		matrixDone := make(chan error, 1)
		matrixConsumer := rabbit.NewConsumer(
			rabbit.New(proxiedURL, "chaos-matrix-"+strconv.Itoa(index)),
			rabbit.New(brokerURL, "chaos-matrix-publisher-"+strconv.Itoa(index)),
			handlerFunc(func(context.Context, string) (consumerapp.Handling, error) {
				matrixHandled.Add(1)
				latenciesMu.Lock()
				latencies = append(latencies, time.Since(started))
				latenciesMu.Unlock()
				return consumerapp.Handling{Disposition: consumerapp.DispositionDone}, nil
			}),
			rabbit.ConsumerConfig{Concurrency: test.concurrency, Prefetch: test.prefetch},
		)
		go func() { matrixDone <- matrixConsumer.Run(matrixCtx, nil) }()
		waitCount(t, 30*time.Second, "backlog drain", &matrixHandled, int64(test.volume))
		waitQueueDepth(t, direct, rabbit.WebhooksQueue, 0)
		latenciesMu.Lock()
		sort.Slice(latencies, func(first, second int) bool { return latencies[first] < latencies[second] })
		p50 := percentile(latencies, 0.50)
		p95 := percentile(latencies, 0.95)
		latenciesMu.Unlock()
		t.Logf("machine=%s/%s go=%s target=%s concurrency=%d prefetch=%d volume=%d elapsed=%s p50=%s p95=%s",
			runtime.GOOS, runtime.GOARCH, runtime.Version(), targetAddress(t, brokerURL, "5672"),
			test.concurrency, test.prefetch, test.volume, time.Since(started), p50, p95)
		cancelMatrix()
		select {
		case err := <-matrixDone:
			if err != nil {
				t.Fatalf("matrix consumer shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("matrix consumer did not stop")
		}
	}
}

func TestLongRabbitMQRelayRecoversAndDrainsBacklog(t *testing.T) {
	brokerURL := requireLocalChaosTarget(t, "TEST_RABBITMQ_URL")
	direct := openRabbit(t, brokerURL, "chaos-relay-direct")
	_ = prepareDedicatedRabbit(t, direct)

	proxy, err := NewTCPProxy(context.Background(), targetAddress(t, brokerURL, "5672"))
	if err != nil {
		t.Fatalf("NewTCPProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	proxy.RejectNew()

	const volume = 50
	repository := newRelayBacklog(t, volume)
	publisher := rabbit.NewPublisher(rabbit.New(
		replaceURLHost(t, brokerURL, proxy.Addr()), "chaos-relay-proxied"))
	t.Cleanup(func() { _ = publisher.Close() })
	service := outboxapp.NewService(repository, publisher, "chaos-relay", outboxapp.Config{
		BatchSize:     5,
		Interval:      50 * time.Millisecond,
		LeaseDuration: 30 * time.Second,
		Backoff:       outboxapp.Backoff{Base: 10 * time.Millisecond, Max: 50 * time.Millisecond},
	})

	runCtx, cancelRun := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stopped := false
	go func() { done <- service.Run(runCtx, nil) }()
	t.Cleanup(func() {
		if stopped {
			return
		}
		cancelRun()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("outbox relay did not stop")
		}
	})

	waitWithBackoff(t, 10*time.Second, "relay retry while RabbitMQ refuses connections",
		func(context.Context) error {
			if repository.retried.Load() == 0 {
				return errors.New("no retry recorded")
			}
			return nil
		})
	started := time.Now()
	proxy.Restore()
	waitWithBackoff(t, 30*time.Second, "relay backlog drain", func(context.Context) error {
		if got := repository.published.Load(); got < volume {
			return fmt.Errorf("published %d of %d", got, volume)
		}
		return nil
	})
	waitQueueDepth(t, direct, rabbit.WebhooksQueue, volume)
	t.Logf("machine=%s/%s go=%s target=%s batch=%d volume=%d retries=%d elapsed=%s",
		runtime.GOOS, runtime.GOARCH, runtime.Version(), targetAddress(t, brokerURL, "5672"),
		service.Config().BatchSize, volume, repository.retried.Load(), time.Since(started))

	cancelRun()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("outbox relay shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("outbox relay did not stop")
	}
	stopped = true
}

type inertInterpreter struct{}

func (inertInterpreter) Interpret(webhookdomain.Event) (consumerapp.SessionOutcome, error) {
	return consumerapp.SessionOutcome{}, errors.New("interpreter must not run while PostgreSQL is unavailable")
}

func (inertInterpreter) Provider() webhookdomain.Provider { return webhookdomain.Stripe }

type handlerFunc func(context.Context, string) (consumerapp.Handling, error)

func (f handlerFunc) Handle(ctx context.Context, eventID string) (consumerapp.Handling, error) {
	return f(ctx, eventID)
}

type relayBacklog struct {
	mu        sync.Mutex
	messages  []webhookdomain.Message
	leased    map[string]bool
	settled   map[string]bool
	retried   atomic.Int64
	published atomic.Int64
}

func newRelayBacklog(t *testing.T, volume int) *relayBacklog {
	t.Helper()
	messages := make([]webhookdomain.Message, 0, volume)
	for index := range volume {
		message := chaosMessage(t)
		message.CorrelationID = "chaos-" + strconv.Itoa(index)
		messages = append(messages, message)
	}
	return &relayBacklog{
		messages: messages,
		leased:   make(map[string]bool, volume),
		settled:  make(map[string]bool, volume),
	}
}

func (r *relayBacklog) Lease(_ context.Context, _ string, batchSize int, _ time.Duration) ([]outboxapp.Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	leases := make([]outboxapp.Lease, 0, batchSize)
	for _, message := range r.messages {
		if r.settled[message.ID] || r.leased[message.ID] {
			continue
		}
		r.leased[message.ID] = true
		leases = append(leases, outboxapp.Lease{Message: message})
		if len(leases) == batchSize {
			break
		}
	}
	return leases, nil
}

func (r *relayBacklog) Published(_ context.Context, _, messageID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.leased, messageID)
	r.settled[messageID] = true
	r.published.Add(1)
	return nil
}

func (r *relayBacklog) Retry(_ context.Context, _, messageID string, _ error, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.leased, messageID)
	r.retried.Add(1)
	return nil
}

func (r *relayBacklog) Failed(_ context.Context, _, messageID string, _ error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.leased, messageID)
	r.settled[messageID] = true
	return nil
}

func requireLocalChaosTarget(t *testing.T, name string) string {
	t.Helper()
	if os.Getenv(chaosAllowedEnv) != "1" {
		t.Skipf("%s=1 is required because this suite purges only its known RabbitMQ queues", chaosAllowedEnv)
	}
	raw := os.Getenv(name)
	if raw == "" {
		t.Skipf("%s is not set", name)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	hostname := parsed.Hostname()
	if hostname != "localhost" && !net.ParseIP(hostname).IsLoopback() {
		t.Fatalf("%s target %q is not loopback; refusing chaos test", name, hostname)
	}
	return raw
}

func targetAddress(t *testing.T, raw, defaultPort string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}
	port := parsed.Port()
	if port == "" {
		port = defaultPort
	}
	return net.JoinHostPort(parsed.Hostname(), port)
}

func replaceURLHost(t *testing.T, raw, address string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}
	parsed.Host = address
	return parsed.String()
}

func openRabbit(t *testing.T, raw, name string) *rabbit.Connection {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := rabbit.Open(ctx, raw, name)
	if err != nil {
		t.Fatalf("open RabbitMQ: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func prepareDedicatedRabbit(t *testing.T, connection *rabbit.Connection) *amqp.Channel {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	channel, err := connection.Channel(ctx)
	if err != nil {
		t.Fatalf("open RabbitMQ channel: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	if err := rabbit.DeclareTopology(channel); err != nil {
		t.Fatalf("declare RabbitMQ topology: %v", err)
	}
	for _, queue := range []string{rabbit.WebhooksQueue, rabbit.DeadLetterQueue} {
		if _, err := channel.QueuePurge(queue, false); err != nil {
			t.Fatalf("purge known queue %s: %v", queue, err)
		}
	}
	t.Cleanup(func() {
		for _, queue := range []string{rabbit.WebhooksQueue, rabbit.DeadLetterQueue} {
			_, _ = channel.QueuePurge(queue, false)
		}
	})
	return channel
}

func publishReferences(t *testing.T, channel *amqp.Channel, offset, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for index := range count {
		eventID := fmt.Sprintf("evt_chaos_%d", offset+index)
		body, err := json.Marshal(map[string]any{
			"messageId":      "msg_" + eventID,
			"schemaVersion":  1,
			"webhookEventId": eventID,
		})
		if err != nil {
			t.Fatalf("encode reference: %v", err)
		}
		if err := channel.PublishWithContext(ctx, rabbit.EventsExchange,
			"payment.webhook.checkout.completed", false, false, amqp.Publishing{
				ContentType: "application/json", DeliveryMode: amqp.Persistent,
				MessageId: "msg_" + eventID, Body: body,
			}); err != nil {
			t.Fatalf("publish reference %d: %v", index, err)
		}
	}
}

func waitQueueDepth(t *testing.T, connection *rabbit.Connection, queue string, want int) {
	t.Helper()
	waitWithBackoff(t, 20*time.Second, "queue "+queue+" depth "+strconv.Itoa(want),
		func(ctx context.Context) error {
			channel, err := connection.Channel(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = channel.Close() }()
			state, err := channel.QueueDeclarePassive(queue, true, false, false, false, nil)
			if err != nil {
				return err
			}
			if state.Messages != want {
				return fmt.Errorf("depth is %d", state.Messages)
			}
			return nil
		})
}

func waitCount(t *testing.T, timeout time.Duration, what string, count *atomic.Int64, want int64) {
	t.Helper()
	waitWithBackoff(t, timeout, what, func(context.Context) error {
		if got := count.Load(); got < want {
			return fmt.Errorf("handled %d, want at least %d", got, want)
		}
		return nil
	})
}

func waitForConsumerError(t *testing.T, errorsSeen <-chan error) {
	t.Helper()
	select {
	case err := <-errorsSeen:
		if err == nil {
			t.Fatal("consumer reported a nil recovery error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("consumer did not report the interrupted connection")
	}
}

func waitWithBackoff(t *testing.T, timeout time.Duration, what string, check func(context.Context) error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	delay := 10 * time.Millisecond
	var lastErr error
	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		lastErr = check(attemptCtx)
		cancel()
		if lastErr == nil {
			return
		}
		timer := time.NewTimer(delay)
		<-timer.C
		if delay *= 2; delay > 500*time.Millisecond {
			delay = 500 * time.Millisecond
		}
	}
	t.Fatalf("timed out waiting for %s; last error: %v", what, lastErr)
}

func percentile(values []time.Duration, quantile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)-1) * quantile)
	return values[index]
}

func chaosMessage(t *testing.T) webhookdomain.Message {
	t.Helper()
	id, err := webhookdomain.NewMessageID()
	if err != nil {
		t.Fatalf("new message ID: %v", err)
	}
	eventID, err := webhookdomain.NewEventID()
	if err != nil {
		t.Fatalf("new event ID: %v", err)
	}
	return webhookdomain.Message{
		ID: id, EventID: eventID, Kind: webhookdomain.KindCheckoutCompleted,
		SchemaVersion: webhookdomain.SchemaVersion,
		RoutingKey:    "payment.webhook.checkout.completed",
		OccurredAt:    time.Now().UTC(), Status: webhookdomain.MessageStatusPending,
	}
}
