package rabbitmq

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Topology names of the payment event pipeline.
const (
	// EventsExchange carries every message produced from a provider event.
	EventsExchange = "payments.events"

	// WebhooksQueue receives the webhook messages the relay publishes.
	WebhooksQueue = "payments.webhooks"

	// WebhookBindingKey binds the queue to the exchange.
	//
	// It uses "#" and not "*" on purpose: routing keys carry the domain kind,
	// as in payment.webhook.checkout.completed, and in a topic exchange "*"
	// matches exactly one word between dots. A "*" here would silently drop
	// every key with more than one segment after the prefix.
	WebhookBindingKey = "payment.webhook.#"

	// DeadLetterExchange and DeadLetterQueue hold messages the consumer could
	// not process within its retry budget.
	DeadLetterExchange = "payments.webhooks.dlx"
	DeadLetterQueue    = "payments.webhooks.dlq"
	deadLetterKey      = "payments.webhooks.dead"
)

// DeclareTopology creates the exchanges, queues and bindings idempotently.
//
// The dead letter is declared now, even though only the consumer will fill it.
// Queue arguments are immutable in RabbitMQ: adding x-dead-letter-exchange to
// an existing queue means deleting and recreating it, which is not an
// acceptable operation on a queue already holding financial messages.
func DeclareTopology(channel *amqp.Channel) error {
	if err := channel.ExchangeDeclare(EventsExchange, amqp.ExchangeTopic,
		true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare events exchange: %w", err)
	}
	if err := channel.ExchangeDeclare(DeadLetterExchange, amqp.ExchangeTopic,
		true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dead letter exchange: %w", err)
	}

	if _, err := channel.QueueDeclare(DeadLetterQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dead letter queue: %w", err)
	}
	if err := channel.QueueBind(DeadLetterQueue, "#", DeadLetterExchange, false, nil); err != nil {
		return fmt.Errorf("bind dead letter queue: %w", err)
	}

	if _, err := channel.QueueDeclare(WebhooksQueue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    DeadLetterExchange,
		"x-dead-letter-routing-key": deadLetterKey,
	}); err != nil {
		return fmt.Errorf("declare webhooks queue: %w", err)
	}
	if err := channel.QueueBind(WebhooksQueue, WebhookBindingKey, EventsExchange, false, nil); err != nil {
		return fmt.Errorf("bind webhooks queue: %w", err)
	}
	return nil
}
