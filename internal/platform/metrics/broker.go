package metrics

import (
	"context"

	"github.com/rmotti/payments-boilerplate/internal/adapters/rabbitmq"
	"go.opentelemetry.io/otel/attribute"
)

// BrokerObserver records what the AMQP consumer does with the message itself,
// as opposed to the event it carries.
//
// Destination and queue are labels because both belong to the declared
// topology: a queue name that is not in the configured set becomes Other,
// which is what keeps a renamed or unexpected destination from creating a
// series of its own.
type BrokerObserver struct {
	metrics *Metrics
	queues  map[string]struct{}
}

// NewBrokerObserver binds the AMQP consumer to its instruments. queues is the
// topology this process declared: the main queue, the dead letter and one per
// retry tier.
func NewBrokerObserver(metrics *Metrics, queues []string) BrokerObserver {
	known := make(map[string]struct{}, len(queues))
	for _, queue := range queues {
		known[queue] = struct{}{}
	}
	return BrokerObserver{metrics: metrics, queues: known}
}

// Rejected records a message whose body could not be read.
func (o BrokerObserver) Rejected(string, error) {
	o.metrics.add(context.Background(), BrokerRejected, 1)
}

// Republished records a confirmed copy sent to a retry tier or the dead letter.
func (o BrokerObserver) Republished(_, destination string, _ int) {
	o.metrics.add(context.Background(), BrokerRepublished, 1,
		Attr(LabelDestination, destinationKind(destination)),
		o.queueAttr(destination))
}

// Failed records a message left unacknowledged for redelivery.
func (o BrokerObserver) Failed(string, error) {
	o.metrics.add(context.Background(), BrokerUnacknowledged, 1)
}

// Redelivered records a delivery the broker flagged as a redelivery.
func (o BrokerObserver) Redelivered(string) {
	o.metrics.add(context.Background(), BrokerRedeliveries, 1)
}

func destinationKind(queue string) string {
	if queue == rabbitmq.DeadLetterQueue {
		return "dead_letter"
	}
	return "retry"
}

// queueAttr keeps queue names bounded by the declared topology.
func (o BrokerObserver) queueAttr(queue string) attribute.KeyValue {
	if _, known := o.queues[queue]; known {
		return attribute.String(LabelQueue, queue)
	}
	return attribute.String(LabelQueue, Other)
}

var (
	_ rabbitmq.ConsumerObserver = BrokerObserver{}
	_ rabbitmq.DeliveryObserver = BrokerObserver{}
)
