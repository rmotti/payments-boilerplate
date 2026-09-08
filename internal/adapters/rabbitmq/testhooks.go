package rabbitmq

import (
	"context"

	amqp "github.com/rabbitmq/amqp091-go"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

// TestHooks replaces or interrupts only protocol boundaries that are otherwise
// impossible to hit deterministically from a test. Hooks are inert unless a
// test explicitly installs them; no environment variable or production
// configuration maps to this type.
//
// Publish replaces one complete publisher attempt, including its confirm.
// Republish does the same for retry and dead-letter copies. The remaining
// callbacks sit immediately after a successful confirm and immediately before
// acknowledgement, respectively.
type TestHooks struct {
	Publish                 func(context.Context, domain.Message) error
	AfterPublishConfirmed   func(domain.Message) error
	Republish               func(context.Context, string, amqp.Delivery) error
	AfterRepublishConfirmed func(string, amqp.Delivery) error
	BeforeAck               func(amqp.Delivery) error
}
