package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	app "github.com/rmotti/payments-boilerplate/internal/application/consumer"
)

// republishTimeout bounds one retry or dead-letter publication, including its
// confirm. It is the budget that stands between a failed message and the ack
// that would drop it.
const republishTimeout = 5 * time.Second

// Handler applies one message and says what should happen to it.
type Handler interface {
	Handle(ctx context.Context, eventID string) (app.Handling, error)
}

// ConsumerConfig tunes delivery and concurrency.
type ConsumerConfig struct {
	// Prefetch bounds how many unacknowledged messages the broker sends. It
	// must be at least Concurrency, or workers sit idle waiting for messages
	// the broker is not allowed to deliver.
	Prefetch int
	// Concurrency is how many messages are applied at once.
	Concurrency int
	// RetryTiers are the delay steps a transient failure walks through.
	RetryTiers []RetryTier
}

// ConsumerObserver reports what the consumer is doing to its messages.
type ConsumerObserver interface {
	// Rejected reports a message that could not be read and will be explicitly
	// republished to the dead letter rather than handled as an inbox reference.
	Rejected(messageID string, cause error)
	// Republished reports a message sent to a retry tier or the dead letter.
	Republished(messageID, destination string, attempts int)
	// Failed reports a message left unacknowledged for redelivery.
	Failed(messageID string, cause error)
}

// Consumer applies webhook messages with manual acknowledgement.
//
// It uses its own connection, separate from the relay's. The Connection
// installs deadlines on the whole socket during an operation, which is right
// for a publisher bounded by a per-attempt budget and fatal for a consumer
// that legitimately waits an unbounded time for the next delivery.
type Consumer struct {
	connection *Connection
	publisher  *Connection
	handler    Handler
	observer   ConsumerObserver
	config     ConsumerConfig
	testHooks  TestHooks
}

// NewConsumer wires a consumer to its broker connections.
//
// publisher is a second connection used only for retry and dead-letter
// republications. Sharing the consuming connection would let a republication's
// socket deadline kill the consuming channel.
func NewConsumer(connection, publisher *Connection, handler Handler, config ConsumerConfig) *Consumer {
	if config.Concurrency < 1 {
		config.Concurrency = 1
	}
	if config.Prefetch < config.Concurrency {
		config.Prefetch = config.Concurrency
	}
	return &Consumer{connection: connection, publisher: publisher, handler: handler, config: config}
}

// WithObserver reports republications and rejections.
func (c *Consumer) WithObserver(observer ConsumerObserver) *Consumer {
	c.observer = observer
	return c
}

// WithTestHooks installs deterministic protocol-boundary hooks. Production
// composition never calls this method.
func (c *Consumer) WithTestHooks(hooks TestHooks) *Consumer {
	c.testHooks = hooks
	return c
}

// Run consumes until ctx is cancelled, reconnecting with backoff.
//
// Each pass opens a channel, declares the topology and consumes until the
// channel dies. A dead channel is normal — a broker restart, a failed
// republication — and the loop simply reconnects; messages that were in flight
// were never acknowledged, so the broker redelivers them.
func (c *Consumer) Run(ctx context.Context, onError func(error)) error {
	const (
		minBackoff = 500 * time.Millisecond
		maxBackoff = 30 * time.Second
	)
	backoff := minBackoff

	for {
		if ctx.Err() != nil {
			return nil
		}

		err := c.consume(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && onError != nil {
			onError(err)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
		if err == nil {
			backoff = minBackoff
		}
	}
}

// consume runs one session against the broker and returns when it ends.
func (c *Consumer) consume(ctx context.Context) error {
	setupCtx, cancelSetup := context.WithTimeout(ctx, handshakeTimeout)
	defer cancelSetup()

	channel, err := c.connection.Channel(setupCtx)
	if err != nil {
		return fmt.Errorf("open consumer channel: %w", err)
	}
	defer func() { _ = channel.Close() }()

	if err := DeclareTopology(channel); err != nil {
		return err
	}
	if err := DeclareRetryTopology(channel, c.config.RetryTiers); err != nil {
		return err
	}
	// Prefetch is what bounds work in flight. Without it the broker pushes the
	// whole queue at once and the concurrency limit means nothing.
	if err := channel.Qos(c.config.Prefetch, 0, false); err != nil {
		return fmt.Errorf("set consumer prefetch: %w", err)
	}

	consumerTag := c.connection.name
	deliveries, err := channel.Consume(WebhooksQueue, consumerTag,
		// autoAck is false: a message is acknowledged only after its effects
		// are committed, never on delivery.
		false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("start consuming: %w", err)
	}

	closed := channel.NotifyClose(make(chan *amqp.Error, 1))

	// A bounded pool, so concurrency is explicit rather than "one goroutine
	// per delivery and hope".
	var workers sync.WaitGroup
	work := make(chan amqp.Delivery)
	workerErrors := make(chan error, c.config.Concurrency)
	for i := 0; i < c.config.Concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for delivery := range work {
				if err := c.handle(ctx, delivery); err != nil {
					// One worker failure ends the whole session. Closing the channel
					// below requeues this delivery and every other unacknowledged one;
					// inbox idempotency makes duplicates safe.
					workerErrors <- err
					return
				}
			}
		}()
	}

	// Shutting down closes work and waits: deliveries already being applied
	// finish their transaction rather than being cut mid-way.
	defer func() {
		close(work)
		workers.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			// Cancel the consumer so the broker stops sending, then let the
			// deferred drain finish what is in flight.
			_ = channel.Cancel(consumerTag, false)
			return nil
		case workerErr := <-workerErrors:
			// A delivery without ack is only redelivered after its channel or
			// connection closes. End this session deliberately and let Run apply
			// reconnect backoff instead of consuming every prefetch slot forever.
			_ = channel.Close()
			return fmt.Errorf("consumer worker failed: %w", workerErr)
		case reason := <-closed:
			if reason != nil {
				return fmt.Errorf("consumer channel closed: %w", reason)
			}
			return errors.New("consumer channel closed")
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("consumer deliveries channel closed")
			}
			select {
			case work <- delivery:
			case <-ctx.Done():
				_ = channel.Cancel(consumerTag, false)
				return nil
			}
		}
	}
}

// handle applies one delivery and settles it. An error means the delivery is
// deliberately left unacknowledged and the consuming session must be closed so
// the broker redelivers it after Run's reconnect backoff.
func (c *Consumer) handle(ctx context.Context, delivery amqp.Delivery) error {
	eventID, err := webhookEventID(delivery.Body)
	if err != nil {
		// The message is unreadable, so no amount of retrying helps and there
		// is no inbox row to record against. Its bytes are still valuable for
		// diagnosis, so publish them explicitly and confirm the copy just like
		// every other dead-letter path.
		if c.observer != nil {
			c.observer.Rejected(delivery.MessageId, err)
		}
		handling := app.Handling{Disposition: app.DispositionDead, Cause: err}
		if deadErr := c.republishToDeadLetter(ctx, delivery, handling); deadErr != nil {
			return c.failed(delivery.MessageId, fmt.Errorf("dead-letter unreadable message: %w", deadErr))
		}
		return nil
	}

	handling, err := c.handler.Handle(ctx, eventID)
	if err != nil {
		// The outcome could not even be recorded. Leaving the message
		// unacknowledged is the only safe answer: the broker redelivers it
		// once this channel closes.
		return c.failed(delivery.MessageId, err)
	}

	var settleErr error
	switch handling.Disposition {
	case app.DispositionDone:
		settleErr = c.ack(delivery)
	case app.DispositionRetry:
		settleErr = c.republishForRetry(ctx, delivery, handling)
	case app.DispositionDead:
		settleErr = c.republishToDeadLetter(ctx, delivery, handling)
	default:
		settleErr = fmt.Errorf("unknown handling disposition %d", handling.Disposition)
	}
	if settleErr != nil {
		return c.failed(delivery.MessageId, settleErr)
	}
	return nil
}

// republishForRetry sends the message into its delay tier, and acknowledges
// the original only once the broker has confirmed the copy.
//
// The order matters: acknowledging first would drop the message if the
// republication then failed. If the republication fails, the original stays
// unacknowledged and comes back through redelivery instead.
func (c *Consumer) republishForRetry(
	ctx context.Context,
	delivery amqp.Delivery,
	handling app.Handling,
) error {
	tier, ok := TierFor(c.config.RetryTiers, handling.Attempts)
	if !ok {
		// No tiers configured: the message cannot wait anywhere, so treat it
		// as dead rather than spinning.
		return c.republishToDeadLetter(ctx, delivery, handling)
	}

	if err := c.republish(ctx, tier.Exchange(), delivery); err != nil {
		return fmt.Errorf("republish for retry: %w", err)
	}
	if c.observer != nil {
		c.observer.Republished(delivery.MessageId, tier.Queue(), handling.Attempts)
	}
	return c.ack(delivery)
}

// republishToDeadLetter sends the message to the dead-letter exchange
// explicitly, rather than relying on nack and the queue's own dead-lettering.
//
// The main queue is classic, and classic-queue dead-lettering republishes
// without publisher confirms: the message leaves the main queue as soon as it
// is published to the DLX and is lost if the DLQ cannot accept it. A message
// lost on the way to the dead letter is the worst possible loss, because that
// is exactly where the things needing human attention go.
func (c *Consumer) republishToDeadLetter(
	ctx context.Context,
	delivery amqp.Delivery,
	handling app.Handling,
) error {
	if err := c.republish(ctx, DeadLetterExchange, delivery); err != nil {
		return fmt.Errorf("republish to dead letter: %w", err)
	}
	if c.observer != nil {
		c.observer.Republished(delivery.MessageId, DeadLetterQueue, handling.Attempts)
	}
	return c.ack(delivery)
}

// republish sends the delivery to an exchange and waits for its confirm.
//
// It uses the publisher connection, never the consuming one, and opens its own
// confirming channel per publication. That is more setup than a long-lived
// channel, but it keeps a failed publication from taking the consuming channel
// down with it, and republications are the exception rather than the rule.
func (c *Consumer) republish(ctx context.Context, exchange string, delivery amqp.Delivery) error {
	if c.testHooks.Republish != nil {
		if err := c.testHooks.Republish(ctx, exchange, delivery); err != nil {
			return err
		}
		if c.testHooks.AfterRepublishConfirmed != nil {
			return c.testHooks.AfterRepublishConfirmed(exchange, delivery)
		}
		return nil
	}

	publishCtx, cancel := context.WithTimeout(ctx, republishTimeout)
	defer cancel()

	return c.publisher.withDeadline(publishCtx, func() error {
		channel, err := c.publisher.channel(publishCtx)
		if err != nil {
			return err
		}
		defer func() { _ = channel.Close() }()

		if err := channel.Confirm(false); err != nil {
			return fmt.Errorf("enable republish confirms: %w", err)
		}
		returns := channel.NotifyReturn(make(chan amqp.Return, 1))

		confirmation, err := channel.PublishWithDeferredConfirmWithContext(
			publishCtx, exchange, delivery.RoutingKey,
			// mandatory: a republication that matches no queue must be an
			// error here, not a silent drop.
			true, false,
			amqp.Publishing{
				ContentType:   delivery.ContentType,
				DeliveryMode:  amqp.Persistent,
				MessageId:     delivery.MessageId,
				Type:          delivery.Type,
				CorrelationId: delivery.CorrelationId,
				Timestamp:     delivery.Timestamp,
				Headers:       delivery.Headers,
				Body:          delivery.Body,
			})
		if err != nil {
			return fmt.Errorf("republish message: %w", err)
		}
		acked, err := confirmation.WaitContext(publishCtx)
		if err != nil {
			return fmt.Errorf("await republish confirm: %w", err)
		}
		if !acked {
			return errors.New("broker refused the republished message")
		}
		select {
		case returned := <-returns:
			return fmt.Errorf("republished message was not routed: %s (%d %s)",
				returned.RoutingKey, returned.ReplyCode, returned.ReplyText)
		default:
		}
		if c.testHooks.AfterRepublishConfirmed != nil {
			if err := c.testHooks.AfterRepublishConfirmed(exchange, delivery); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *Consumer) ack(delivery amqp.Delivery) error {
	if c.testHooks.BeforeAck != nil {
		if err := c.testHooks.BeforeAck(delivery); err != nil {
			return err
		}
	}
	// multiple is false: each message is settled on its own, because workers
	// finish out of order and a cumulative ack would settle messages that are
	// still being applied.
	if err := delivery.Ack(false); err != nil {
		return fmt.Errorf("acknowledge message: %w", err)
	}
	return nil
}

func (c *Consumer) failed(messageID string, cause error) error {
	if c.observer != nil {
		c.observer.Failed(messageID, cause)
	}
	return cause
}

// messageBodyRef is the part of the published message the consumer needs. The
// message carries a reference, never the provider payload: the canonical copy
// is read from the inbox. See ADR 0011.
type messageBodyRef struct {
	WebhookEventID string `json:"webhookEventId"`
}

func webhookEventID(body []byte) (string, error) {
	var parsed messageBodyRef
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode message body: %w", err)
	}
	if parsed.WebhookEventID == "" {
		return "", errors.New("message carries no webhook event reference")
	}
	return parsed.WebhookEventID, nil
}
