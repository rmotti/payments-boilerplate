package rabbitmq

import (
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// RetryExchangePrefix namespaces the per-tier retry exchanges.
const RetryExchangePrefix = "payments.webhooks.retry."

// RetryTier is one delay step. Each tier owns an exchange and a queue, because
// a queue's TTL expires messages in arrival order: a single queue holding
// mixed delays would let a long wait at the head block every short one behind
// it, which destroys the backoff.
type RetryTier struct {
	Delay time.Duration
}

// Name is the tier's suffix, derived from its delay so the topology reads the
// same in code and in the management UI.
func (t RetryTier) Name() string {
	return strings.ReplaceAll(t.Delay.String(), "µ", "u")
}

// Exchange is the fanout the consumer republishes into.
//
// Fanout matters: the message has to come back to payments.events carrying its
// original routing key, or it will not match the payment.webhook.# binding.
// Publishing to a fanout ignores the routing key for routing but leaves it on
// the message, and dead-lettering preserves it when the queue sets no
// x-dead-letter-routing-key.
func (t RetryTier) Exchange() string { return RetryExchangePrefix + t.Name() }

// Queue holds the message for the tier's delay.
func (t RetryTier) Queue() string { return RetryExchangePrefix + t.Name() }

// DeclareRetryTopology creates the per-tier exchanges and queues idempotently.
//
// Every object here is new. The main queue is never touched: its arguments are
// immutable in RabbitMQ, and ADR 0012 declared its dead-letter exchange
// precisely so it would never have to be deleted and recreated.
//
// The queues are quorum with at-least-once dead-lettering, because the return
// trip to payments.events is itself a dead-letter republication. A classic
// queue republishes without publisher confirms and drops the message if the
// target cannot accept it; a quorum queue with this strategy confirms
// internally and only then removes it. That strategy requires reject-publish
// overflow, so a full retry queue refuses new messages rather than silently
// dropping the oldest — the consumer sees the refusal and leaves the original
// message unacknowledged.
func DeclareRetryTopology(channel *amqp.Channel, tiers []RetryTier) error {
	for _, tier := range tiers {
		if tier.Delay <= 0 {
			return fmt.Errorf("retry tier must have a positive delay, got %s", tier.Delay)
		}
		exchange := tier.Exchange()
		queue := tier.Queue()

		if err := channel.ExchangeDeclare(exchange, amqp.ExchangeFanout,
			true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare retry exchange %s: %w", exchange, err)
		}
		if _, err := channel.QueueDeclare(queue, true, false, false, false, amqp.Table{
			"x-queue-type": "quorum",
			// Expiry is what produces the delay; the message then dead-letters
			// back to the events exchange with its original routing key.
			"x-message-ttl":          tier.Delay.Milliseconds(),
			"x-dead-letter-exchange": EventsExchange,
			"x-dead-letter-strategy": "at-least-once",
			"x-overflow":             "reject-publish",
		}); err != nil {
			return fmt.Errorf("declare retry queue %s: %w", queue, err)
		}
		if err := channel.QueueBind(queue, "", exchange, false, nil); err != nil {
			return fmt.Errorf("bind retry queue %s: %w", queue, err)
		}
	}
	return nil
}

// TierFor picks the delay tier for an attempt count. The last tier repeats
// once the ladder is exhausted, so a message keeps waiting the longest delay
// until the consumer's attempt budget stops it.
func TierFor(tiers []RetryTier, attempts int) (RetryTier, bool) {
	if len(tiers) == 0 {
		return RetryTier{}, false
	}
	index := attempts - 1
	if index < 0 {
		index = 0
	}
	if index >= len(tiers) {
		index = len(tiers) - 1
	}
	return tiers[index], true
}
