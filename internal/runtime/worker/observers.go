package worker

import (
	consumerapp "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	"go.uber.org/zap"
)

// relayObserver turns relay findings into logs. A stuck message keeps being
// retried; surfacing it is what lets an operator notice before a customer does.
type relayObserver struct{ logger *zap.Logger }

func (o relayObserver) Stuck(messageID string, attempts int, cause error) {
	o.logger.Warn("outbox message is not going through",
		zap.String("component", "outbox_relay"),
		zap.String("message_id", messageID),
		zap.Int("attempts", attempts),
		logging.SanitizedError(cause),
	)
}

func (o relayObserver) Abandoned(messageID string, attempts int, cause error) {
	o.logger.Error("outbox message abandoned after a permanent failure",
		zap.String("component", "outbox_relay"),
		zap.String("message_id", messageID),
		zap.Int("attempts", attempts),
		logging.SanitizedError(cause),
	)
}

func (o relayObserver) LeaseLost(messageID string, cause error) {
	o.logger.Warn("outbox lease expired before the outcome could be recorded",
		zap.String("component", "outbox_relay"),
		zap.String("message_id", messageID),
		logging.SanitizedError(cause),
	)
}

// consumerObserver turns consumer decisions into logs. Metrics for these
// belong to Phase 4; the relay set the same precedent.
type consumerObserver struct{ logger *zap.Logger }

func (o consumerObserver) Applied(eventID string, effect consumerapp.Effect) {
	o.logger.Info("webhook event applied",
		zap.String("component", "consumer"),
		zap.String("webhook_event_id", eventID),
		zap.String("attempt_status", string(effect.AttemptStatus)),
		zap.String("payment_status", string(effect.PaymentStatus)),
		zap.Bool("order_paid", effect.OrderPaid),
	)
}

func (o consumerObserver) NoOp(eventID, note string) {
	o.logger.Info("webhook event produced no change",
		zap.String("component", "consumer"),
		zap.String("webhook_event_id", eventID),
		zap.String("reason", note),
	)
}

func (o consumerObserver) Retrying(eventID string, attempts int, cause error) {
	o.logger.Warn("webhook event will be retried",
		zap.String("component", "consumer"),
		zap.String("webhook_event_id", eventID),
		zap.Int("attempts", attempts),
		logging.SanitizedError(cause),
	)
}

func (o consumerObserver) DeadLettered(eventID string, attempts int, cause error) {
	o.logger.Error("webhook event sent to the dead-letter queue",
		zap.String("component", "consumer"),
		zap.String("webhook_event_id", eventID),
		zap.Int("attempts", attempts),
		logging.SanitizedError(cause),
	)
}

// brokerObserver reports what the consumer did with the message itself, as
// opposed to the event it carried.
type brokerObserver struct{ logger *zap.Logger }

func (o brokerObserver) Rejected(messageID string, cause error) {
	o.logger.Error("message could not be read and was rejected",
		zap.String("component", "consumer"),
		zap.String("message_id", messageID),
		logging.SanitizedError(cause),
	)
}

func (o brokerObserver) Republished(messageID, destination string, attempts int) {
	o.logger.Info("message republished",
		zap.String("component", "consumer"),
		zap.String("message_id", messageID),
		zap.String("destination", destination),
		zap.Int("attempts", attempts),
	)
}

func (o brokerObserver) Failed(messageID string, cause error) {
	// Not acknowledged, so the broker still owns it. This is the safe outcome
	// of an unknown one, but it must be visible.
	o.logger.Error("message left unacknowledged for redelivery",
		zap.String("component", "consumer"),
		zap.String("message_id", messageID),
		logging.SanitizedError(cause),
	)
}
