package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	app "github.com/rmotti/payments-boilerplate/internal/application/outbox"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"

	amqp "github.com/rabbitmq/amqp091-go"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

const testBrokerURLEnv = "TEST_RABBITMQ_URL"

func openTestBroker(t *testing.T) *Connection {
	t.Helper()

	url := os.Getenv(testBrokerURLEnv)
	if url == "" {
		t.Skipf("%s is not set", testBrokerURLEnv)
	}
	connection, err := Open(context.Background(), url, "publisher-test")
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Errorf("close broker: %v", err)
		}
	})
	return connection
}

func testMessage(kind domain.Kind) domain.Message {
	id, _ := domain.NewMessageID()
	eventID, _ := domain.NewEventID()
	return domain.Message{
		ID: id, EventID: eventID, Kind: kind, SchemaVersion: domain.SchemaVersion,
		RoutingKey: "payment.webhook." + string(kind), CorrelationID: "corr-test",
		OccurredAt: time.Now().UTC().Truncate(time.Second), Status: domain.MessageStatusPending,
	}
}

// The declaration must be safe to run on every worker start.
func TestDeclareTopologyIsIdempotent(t *testing.T) {
	connection := openTestBroker(t)

	for i := 0; i < 3; i++ {
		channel, err := connection.connection.Channel()
		if err != nil {
			t.Fatalf("open channel: %v", err)
		}
		if err := DeclareTopology(channel); err != nil {
			t.Fatalf("DeclareTopology() attempt %d error = %v", i, err)
		}
		_ = channel.Close()
	}
}

// Routing keys carry the domain kind and have more than one segment after the
// prefix. A "*" binding would silently drop them, so this pins the "#".
func TestPublishedMessageReachesTheQueue(t *testing.T) {
	connection := openTestBroker(t)

	publisher := NewPublisher(connection)
	t.Cleanup(func() { _ = publisher.Close() })

	channel, err := connection.connection.Channel()
	if err != nil {
		t.Fatalf("open consume channel: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	if _, err := channel.QueuePurge(WebhooksQueue, false); err != nil {
		t.Fatalf("purge queue: %v", err)
	}

	deliveries, err := channel.Consume(WebhooksQueue, "", true, false, false, false, nil)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}

	message := testMessage(domain.KindCheckoutCompleted)
	if err := publisher.Publish(context.Background(), message); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	select {
	case delivery := <-deliveries:
		if delivery.MessageId != message.ID {
			t.Fatalf("MessageId = %q, want %q", delivery.MessageId, message.ID)
		}
		if delivery.DeliveryMode != amqp.Persistent {
			t.Fatalf("DeliveryMode = %d, want persistent", delivery.DeliveryMode)
		}
		var body messageBody
		if err := json.Unmarshal(delivery.Body, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.WebhookEventID != message.EventID {
			t.Fatalf("webhookEventId = %q, want the event reference", body.WebhookEventID)
		}
		if body.SchemaVersion != domain.SchemaVersion {
			t.Fatalf("schemaVersion = %d, want %d", body.SchemaVersion, domain.SchemaVersion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("published message never reached the queue; check the binding key")
	}
}

// Every handled kind must route to the queue. This is the regression guard for
// the "*" versus "#" mistake: several kinds have two segments after the prefix.
func TestEveryKindRoutesToTheQueue(t *testing.T) {
	connection := openTestBroker(t)

	publisher := NewPublisher(connection)
	t.Cleanup(func() { _ = publisher.Close() })

	channel, err := connection.connection.Channel()
	if err != nil {
		t.Fatalf("open consume channel: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	if _, err := channel.QueuePurge(WebhooksQueue, false); err != nil {
		t.Fatalf("purge queue: %v", err)
	}

	kinds := []domain.Kind{
		domain.KindCheckoutCompleted,
		domain.KindCheckoutPaymentSucceeded,
		domain.KindCheckoutPaymentFailed,
		domain.KindCheckoutExpired,
	}
	for _, kind := range kinds {
		if err := publisher.Publish(context.Background(), testMessage(kind)); err != nil {
			t.Fatalf("Publish(%s) error = %v", kind, err)
		}
	}

	deliveries, err := channel.Consume(WebhooksQueue, "", true, false, false, false, nil)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	received := 0
	for received < len(kinds) {
		select {
		case <-deliveries:
			received++
		case <-time.After(5 * time.Second):
			t.Fatalf("received %d of %d messages; a routing key was not bound",
				received, len(kinds))
		}
	}
}

// The publisher must recover from a dead channel: a relay that gave up on the
// first protocol error would stop publishing until the process restarted.
func TestPublisherReopensClosedChannel(t *testing.T) {
	connection := openTestBroker(t)

	publisher := NewPublisher(connection)
	t.Cleanup(func() { _ = publisher.Close() })

	if err := publisher.Publish(context.Background(), testMessage(domain.KindCheckoutCompleted)); err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}

	publisher.mu.Lock()
	_ = publisher.channel.Close()
	publisher.mu.Unlock()

	if err := publisher.Publish(context.Background(), testMessage(domain.KindCheckoutExpired)); err != nil {
		t.Fatalf("Publish() after channel loss error = %v, want a reopened channel", err)
	}
}

// A publisher confirm only proves the exchange took the message, not that any
// queue did. Without mandatory publishing, a message whose routing key matches
// no binding is acked and silently dropped — and the relay would mark it
// published. This pins that it is reported instead.
func TestUnroutableMessageIsNotReportedAsPublished(t *testing.T) {
	connection := openTestBroker(t)

	publisher := NewPublisher(connection)
	t.Cleanup(func() { _ = publisher.Close() })

	message := testMessage(domain.KindCheckoutCompleted)
	// A key the webhooks queue is not bound to.
	message.RoutingKey = "payment.unrouted.nowhere"

	err := publisher.Publish(context.Background(), message)
	if err == nil {
		t.Fatal("Publish() error = nil, want an unroutable message to be reported")
	}
	if !errors.Is(err, app.ErrNotRouted) {
		t.Fatalf("Publish() error = %v, want ErrNotRouted", err)
	}
	// Topology problems are transient: a binding may be restored.
	if app.IsPermanent(err) {
		t.Fatal("an unroutable message must stay retryable")
	}
}

// A return left by one publish must not be blamed on the next message.
func TestPublisherRecoversAfterUnroutableMessage(t *testing.T) {
	connection := openTestBroker(t)

	publisher := NewPublisher(connection)
	t.Cleanup(func() { _ = publisher.Close() })

	unroutable := testMessage(domain.KindCheckoutCompleted)
	unroutable.RoutingKey = "payment.unrouted.nowhere"
	if err := publisher.Publish(context.Background(), unroutable); err == nil {
		t.Fatal("expected the unroutable publish to fail")
	}

	if err := publisher.Publish(context.Background(), testMessage(domain.KindCheckoutCompleted)); err != nil {
		t.Fatalf("Publish() after an unroutable message error = %v, want success", err)
	}
}

// amqp091-go never reconnects on its own. If the connection dies, opening
// another channel on it fails forever, so the relay must redial.
func TestPublisherRecoversFromLostConnection(t *testing.T) {
	connection := openTestBroker(t)

	publisher := NewPublisher(connection)
	t.Cleanup(func() { _ = publisher.Close() })

	if err := publisher.Publish(context.Background(), testMessage(domain.KindCheckoutCompleted)); err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}

	// Kill the whole connection, not just the channel.
	connection.mu.Lock()
	_ = connection.connection.Close()
	connection.mu.Unlock()

	// The first attempt after the loss may fail on the dead channel; what
	// matters is that the relay recovers rather than failing forever.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		lastErr = publisher.Publish(context.Background(), testMessage(domain.KindCheckoutExpired))
		if lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("Publish() never recovered after the connection was lost: %v", lastErr)
}

// The worker must start even with the broker down: messages are already
// durable in PostgreSQL, and refusing to boot would only mean the relay is
// missing when the broker comes back.
func TestPublisherIsUsableBeforeTheBrokerIsReachable(t *testing.T) {
	t.Parallel()

	// A port nothing is listening on: this fails instantly, which is exactly
	// why it cannot prove anything about the timeout. See the test below.
	connection := New("amqp://guest:guest@127.0.0.1:59999/", "unreachable-test")
	publisher := NewPublisher(connection)
	t.Cleanup(func() { _ = publisher.Close() })

	err := publisher.Publish(context.Background(), testMessage(domain.KindCheckoutCompleted))
	if err == nil {
		t.Fatal("Publish() error = nil, want a failure while the broker is unreachable")
	}
	// It must be retryable: an unreachable broker is the textbook transient
	// condition, and treating it as permanent would discard the event.
	if app.IsPermanent(err) {
		t.Fatalf("Publish() error = %v, want a transient failure", err)
	}
}

// The configuration refuses a lease too short to cover a batch, and that check
// multiplies by its own copy of this budget. If the two drift, the check stops
// protecting the lease, so pin them together.
func TestPublishTimeoutMatchesTheConfiguredBudget(t *testing.T) {
	t.Parallel()

	if PublishTimeout != config.PublishAttemptBudget {
		t.Fatalf("PublishTimeout = %s but config.PublishAttemptBudget = %s; "+
			"the lease sizing check would no longer protect the lease",
			PublishTimeout, config.PublishAttemptBudget)
	}
}

// A closed port refuses instantly, so it cannot show whether the deadline is
// enforced. A host that accepts the TCP connection and then says nothing does:
// without the budget covering the dial and handshake, this would hang for the
// operating system's timeout rather than PublishTimeout.
//
// It matters because the lease is sized as batch x PublishTimeout. If one
// attempt can exceed that, a batch can outlive its lease and let a second
// relay take a message this one is still publishing.
func TestPublishTimeoutCoversConnectionSetup(t *testing.T) {
	t.Parallel()

	// A listener that accepts and never speaks AMQP.
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	// Accepted connections are closed by the accept loop itself when the
	// listener shuts down, so the test never races the goroutine over them.
	var acceptDone sync.WaitGroup
	acceptDone.Add(1)
	go func() {
		defer acceptDone.Done()
		var conns []net.Conn
		defer func() {
			for _, conn := range conns {
				_ = conn.Close()
			}
		}()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Accept and stay silent: never speak AMQP.
			conns = append(conns, conn)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		acceptDone.Wait()
	})

	connection := New("amqp://guest:guest@"+listener.Addr().String()+"/", "silent-test")
	publisher := NewPublisher(connection)
	t.Cleanup(func() { _ = publisher.Close() })

	started := time.Now()
	err = publisher.Publish(context.Background(), testMessage(domain.KindCheckoutCompleted))
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("Publish() error = nil, want a failure against a silent broker")
	}
	// Generous margin: the point is that it is bounded at all, not its precision.
	if elapsed > PublishTimeout+3*time.Second {
		t.Fatalf("Publish() took %s, want it bounded by PublishTimeout (%s)", elapsed, PublishTimeout)
	}
	if app.IsPermanent(err) {
		t.Fatalf("Publish() error = %v, want a transient failure", err)
	}
}

// The AMQP heartbeat loop is allowed to extend its read deadline. During a
// publish it must not extend it beyond the operation budget, or a synchronous
// RPC can remain blocked after PublishTimeout has elapsed.
func TestOperationDeadlineCapsDriverDeadlineChanges(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	transport := newDeadlineConn(client)
	deadline := time.Now().Add(50 * time.Millisecond)
	if err := transport.limit(deadline); err != nil {
		t.Fatalf("limit: %v", err)
	}
	// Simulate the heartbeat extending its own deadline after the operation
	// limit was installed. The wrapper must retain the earlier limit.
	if err := transport.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	started := time.Now()
	if _, err := transport.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read() error = nil, want the operation deadline to interrupt it")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Read() took %s, want the operation deadline to win", elapsed)
	}
}
