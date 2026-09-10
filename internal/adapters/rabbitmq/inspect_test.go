package rabbitmq

import (
	"context"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The depth read is what the metrics sampler publishes, so it has to report
// the messages actually waiting rather than the ones ever published.
func TestQueueDepthsReportsReadyMessages(t *testing.T) {
	connection := openTestBroker(t)
	channel := prepareBroker(t, connection, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	depths, err := QueueDepths(ctx, connection, []string{WebhooksQueue, DeadLetterQueue})
	if err != nil {
		t.Fatalf("QueueDepths() on empty queues error = %v", err)
	}
	if depths[WebhooksQueue] != 0 || depths[DeadLetterQueue] != 0 {
		t.Fatalf("purged queues report %v, want zero depths", depths)
	}

	if err := channel.PublishWithContext(ctx, DeadLetterExchange, "payments.webhooks.dead",
		false, false, amqp.Publishing{
			ContentType: "application/json", DeliveryMode: amqp.Persistent,
			MessageId: "msg_depth", Body: []byte(`{"webhookEventId":"evt_depth"}`),
		}); err != nil {
		t.Fatalf("publish to dead letter: %v", err)
	}

	waitFor(t, 10*time.Second, "the dead letter to hold one message", func() bool {
		depths, err := QueueDepths(ctx, connection, []string{DeadLetterQueue})
		return err == nil && depths[DeadLetterQueue] == 1
	})
}

// A queue the topology does not define must be an error the sampler can
// report, not a silent zero that would hide a renamed queue.
func TestQueueDepthsFailsOnAnUndeclaredQueue(t *testing.T) {
	connection := openTestBroker(t)
	prepareBroker(t, connection, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := QueueDepths(ctx, connection, []string{"payments.webhooks.does-not-exist"}); err == nil {
		t.Fatal("QueueDepths() on a missing queue error = nil, want a failure")
	}

	// The failed passive declaration closed its own channel, not the
	// connection: the next inspection has to work.
	if _, err := QueueDepths(ctx, connection, []string{WebhooksQueue}); err != nil {
		t.Fatalf("QueueDepths() after a missing queue error = %v", err)
	}
}

// Opening a channel is only the first synchronous AMQP operation. The same
// deadline must remain installed while the passive declaration waits for its
// reply, otherwise a broker that accepts connections but stops answering can
// hold the metrics sampler and worker shutdown indefinitely.
func TestInspectionChannelKeepsDeadlineForTheWholeOperation(t *testing.T) {
	connection := openTestBroker(t)
	// This test deliberately expires the transport deadline. The first AMQP
	// close handshake may therefore return that expected timeout; consume it
	// here so the shared cleanup only verifies that Close is idempotent.
	defer func() { _ = connection.Close() }()
	prepareBroker(t, connection, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := connection.withChannel(ctx, func(channel *amqp.Channel) error {
		<-ctx.Done()
		_, err := channel.QueueDeclarePassive(WebhooksQueue, true, false, false, false, nil)
		return err
	})
	if err == nil {
		t.Fatal("inspection after its deadline error = nil, want the socket deadline to stop it")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("inspection took %s after a 50ms deadline", elapsed)
	}
}
