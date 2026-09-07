package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	app "github.com/rmotti/payments-boilerplate/internal/application/outbox"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

// PublishTimeout bounds one whole publish attempt: redialing, declaring the
// topology, sending, and waiting for the confirm.
//
// It is exported because the lease has to be long enough to cover a batch of
// them, and the configuration refuses to start when it is not.
const PublishTimeout = 5 * time.Second

// messageBody is the wire format. It carries a reference to the stored event,
// never the provider payload: the consumer reads the canonical copy from the
// inbox. See ADR 0011.
type messageBody struct {
	MessageID      string    `json:"messageId"`
	Type           string    `json:"type"`
	SchemaVersion  int       `json:"schemaVersion"`
	OccurredAt     time.Time `json:"occurredAt"`
	CorrelationID  string    `json:"correlationId"`
	WebhookEventID string    `json:"webhookEventId"`
}

// Publisher publishes outbox messages and waits for publisher confirms.
type Publisher struct {
	connection *Connection

	// mu serializes access to the channel. An AMQP channel is not safe for
	// concurrent use, and confirms are matched to publishes by sequence.
	mu      sync.Mutex
	channel *amqp.Channel
	returns chan amqp.Return
}

// NewPublisher creates a publisher bound to a connection.
//
// It does not dial: the channel, the confirm mode and the topology are
// established on first use and re-established whenever they are lost, so a
// broker that is down right now never stops the worker from starting.
func NewPublisher(connection *Connection) *Publisher {
	return &Publisher{connection: connection}
}

// Warm establishes the channel and topology ahead of the first publish, so a
// healthy broker does not spend a message's budget on setup.
func (p *Publisher) Warm(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	warmCtx, cancel := context.WithTimeout(ctx, PublishTimeout)
	defer cancel()
	return p.connection.withDeadline(warmCtx, func() error {
		_, err := p.ensureChannel(warmCtx)
		return err
	})
}

// ensureChannel returns a channel in confirm mode, reopening it if needed.
// The caller must hold mu.
//
// It asks the Connection for the channel rather than holding one of its own,
// so a broker restart is recovered by redialing instead of failing forever on
// a dead connection. The context bounds that redial, which is why it has to be
// inside the caller's deadline rather than before it.
func (p *Publisher) ensureChannel(ctx context.Context) (*amqp.Channel, error) {
	if p.channel != nil && !p.channel.IsClosed() {
		return p.channel, nil
	}

	channel, err := p.connection.channel(ctx)
	if err != nil {
		return nil, err
	}
	// Confirm mode is what makes "published" mean "the broker has it".
	if err := channel.Confirm(false); err != nil {
		_ = channel.Close()
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	if err := DeclareTopology(channel); err != nil {
		_ = channel.Close()
		return nil, err
	}

	// A confirm only proves the exchange accepted the message, not that any
	// queue took it. With mandatory publishes, an unroutable message comes
	// back here before its confirm, which is how we tell the two apart.
	p.returns = channel.NotifyReturn(make(chan amqp.Return, 1))
	p.channel = channel
	return channel, nil
}

// Publish sends one message and returns only after the broker confirms it and
// no return says it was unroutable.
//
// Any other outcome is an error, which leaves the outbox row for a later
// attempt. That is deliberate: an unconfirmed publish cannot be distinguished
// from a lost one, and republishing a message the broker did receive is
// harmless, while dropping one is not.
func (p *Publisher) Publish(ctx context.Context, message domain.Message) error {
	body, err := json.Marshal(messageBody{
		MessageID:      message.ID,
		Type:           string(message.Kind),
		SchemaVersion:  message.SchemaVersion,
		OccurredAt:     message.OccurredAt.UTC(),
		CorrelationID:  message.CorrelationID,
		WebhookEventID: message.EventID,
	})
	if err != nil {
		// Nothing about retrying changes an unencodable message.
		return app.Permanent(fmt.Errorf("encode outbox message: %w", err))
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// The budget covers the whole attempt. Establishing the connection is part
	// of publishing, and leaving it outside the deadline let one unreachable
	// broker hold a message for as long as the OS took to give up.
	attemptCtx, cancel := context.WithTimeout(ctx, PublishTimeout)
	defer cancel()

	return p.connection.withDeadline(attemptCtx, func() error {
		channel, err := p.ensureChannel(attemptCtx)
		if err != nil {
			return err
		}
		p.drainReturns()

		confirmation, err := channel.PublishWithDeferredConfirmWithContext(
			attemptCtx, EventsExchange, message.RoutingKey,
			// mandatory: tell us when no queue matched instead of dropping it.
			true,
			// immediate: not supported by RabbitMQ.
			false,
			amqp.Publishing{
				ContentType:   "application/json",
				DeliveryMode:  amqp.Persistent,
				MessageId:     message.ID,
				Type:          string(message.Kind),
				CorrelationId: message.CorrelationID,
				Timestamp:     message.OccurredAt.UTC(),
				Body:          body,
			})
		if err != nil {
			p.discardChannel()
			return fmt.Errorf("publish outbox message: %w", err)
		}

		acked, err := confirmation.WaitContext(attemptCtx)
		if err != nil {
			p.discardChannel()
			return fmt.Errorf("%w: %w", app.ErrNotConfirmed, err)
		}
		if !acked {
			// A nack means the broker explicitly refused responsibility.
			return app.ErrNotConfirmed
		}

		// The return, if any, arrives before the confirm, so by now it is here.
		if returned, ok := p.takeReturn(); ok {
			return fmt.Errorf("%w: %s (%d %s)", app.ErrNotRouted,
				returned.RoutingKey, returned.ReplyCode, returned.ReplyText)
		}
		return nil
	})
}

// drainReturns clears returns left by an earlier publish, so a stale one is
// never blamed on the message about to be sent.
func (p *Publisher) drainReturns() {
	for {
		select {
		case <-p.returns:
		default:
			return
		}
	}
}

func (p *Publisher) takeReturn() (amqp.Return, bool) {
	select {
	case returned, ok := <-p.returns:
		return returned, ok
	default:
		return amqp.Return{}, false
	}
}

func (p *Publisher) discardChannel() {
	if p.channel != nil {
		_ = p.channel.Close()
		p.channel = nil
	}
	p.returns = nil
}

// Close releases the publisher channel. The connection is owned by the caller.
func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.channel == nil || p.channel.IsClosed() {
		return nil
	}
	return p.channel.Close()
}

var _ app.Publisher = (*Publisher)(nil)
