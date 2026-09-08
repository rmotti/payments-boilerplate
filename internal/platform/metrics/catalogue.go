// Package metrics builds the application's OpenTelemetry instruments and the
// observers that feed them.
//
// The application and adapter layers never import OpenTelemetry. They expose
// observer interfaces in their own vocabulary; this package implements those
// interfaces and turns what it hears into counters, histograms and gauges.
// The composition roots in internal/runtime wire the two together.
//
// Every attribute recorded here passes through an allowlist. Identifiers,
// correlation ids, secrets, error messages and free-form paths are never
// labels; a value outside the allowlist becomes "other". ADR 0015 is the
// authoritative definition of every instrument and every label.
package metrics

// Kind is the OpenTelemetry instrument type.
type Kind string

// Instrument kinds.
const (
	KindCounter           Kind = "counter"
	KindHistogram         Kind = "histogram"
	KindGauge             Kind = "gauge"
	KindObservableCounter Kind = "observable_counter"
)

// Instrument describes one entry of the catalogue.
type Instrument struct {
	Name        string
	Description string
	Unit        string
	Kind        Kind
	// Labels lists the attribute keys the instrument may carry. Every key is
	// one of the allowlisted label keys, and every value recorded under it is
	// one of that key's allowed values.
	Labels []string
}

// Instrument names. They are the stable contract with dashboards and alerts.
const (
	WebhookReceived         = "payment.webhook.received"
	WebhookInvalidSignature = "payment.webhook.invalid_signature"
	WebhookDuplicate        = "payment.webhook.duplicate"
	WebhookReceiveDuration  = "payment.webhook.receive.duration"
	WebhookReceiveFailures  = "payment.webhook.receive.failures"

	ProviderRequestDuration = "payment.provider.request.duration"
	ProviderRequestFailures = "payment.provider.request.failures"

	IdempotencyReplays   = "payment.idempotency.replays"
	IdempotencyConflicts = "payment.idempotency.conflicts"

	StateTransitions = "payment.state.transitions"

	OutboxPublishDuration   = "outbox.publish.duration"
	OutboxPublishFailures   = "outbox.publish.failures"
	OutboxRelayCycleTime    = "outbox.relay.cycle.duration"
	OutboxRelayCycleFailure = "outbox.relay.cycle.failures"
	OutboxRelayStuck        = "outbox.relay.stuck"
	OutboxRelayAbandoned    = "outbox.relay.abandoned"
	OutboxRelayLeaseLost    = "outbox.relay.lease_lost"

	ConsumerHandleDuration = "payment.consumer.handle.duration"
	ConsumerNoOps          = "payment.consumer.no_ops"
	ConsumerFailures       = "payment.consumer.failures"
	ConsumerRetries        = "payment.consumer.retries"
	ConsumerDeadLetters    = "payment.consumer.dead_letters"

	BrokerRedeliveries   = "rabbitmq.consumer.redeliveries"
	BrokerRepublished    = "rabbitmq.consumer.republished"
	BrokerRejected       = "rabbitmq.consumer.rejected"
	BrokerUnacknowledged = "rabbitmq.consumer.unacknowledged"

	OutboxPending   = "outbox.pending"
	OutboxOldestAge = "outbox.oldest.age"
	InboxPending    = "webhook.inbox.pending"
	InboxFailed     = "webhook.inbox.failed"
	InboxOldestAge  = "webhook.inbox.oldest.age"
	QueueDepth      = "rabbitmq.queue.depth"

	PoolConnections = "database.pool.connections"
	PoolMaxOpen     = "database.pool.max_open"
	PoolWaits       = "database.pool.waits"

	SamplerAge      = "metrics.sampler.age"
	SamplerFailures = "metrics.sampler.failures"

	HTTPRateLimited = "http.server.rate_limited"
)

// catalogue is the single definition of every instrument this package
// creates. Construction reads from it, and the tests compare what a reader
// collects against it, so an instrument cannot exist without being described.
var catalogue = []Instrument{
	{Name: WebhookReceived, Kind: KindCounter, Unit: "{event}",
		Description: "Provider events received, by outcome.",
		Labels:      []string{LabelProvider, LabelEventKind, LabelOutcome}},
	{Name: WebhookInvalidSignature, Kind: KindCounter, Unit: "{request}",
		Description: "Webhook requests rejected because the signature could not be verified.",
		Labels:      []string{LabelProvider}},
	{Name: WebhookDuplicate, Kind: KindCounter, Unit: "{event}",
		Description: "Provider events received again after being stored.",
		Labels:      []string{LabelProvider, LabelEventKind}},
	{Name: WebhookReceiveDuration, Kind: KindHistogram, Unit: "s",
		Description: "Time to verify and durably store one provider event.",
		Labels:      []string{LabelProvider, LabelOutcome}},
	{Name: WebhookReceiveFailures, Kind: KindCounter, Unit: "{request}",
		Description: "Webhook requests that ended without a stored event.",
		Labels:      []string{LabelProvider, LabelReason}},

	{Name: ProviderRequestDuration, Kind: KindHistogram, Unit: "s",
		Description: "Time spent in one payment provider request.",
		Labels:      []string{LabelProvider, LabelOperation, LabelOutcome}},
	{Name: ProviderRequestFailures, Kind: KindCounter, Unit: "{request}",
		Description: "Payment provider requests that failed, by normalized outcome.",
		Labels:      []string{LabelProvider, LabelOperation, LabelOutcome}},

	{Name: IdempotencyReplays, Kind: KindCounter, Unit: "{request}",
		Description: "Requests answered from an earlier result because the idempotency key was seen before.",
		Labels:      []string{LabelOperation}},
	{Name: IdempotencyConflicts, Kind: KindCounter, Unit: "{request}",
		Description: "Requests refused because the idempotency key was reused with a different payload.",
		Labels:      []string{LabelOperation}},

	{Name: StateTransitions, Kind: KindCounter, Unit: "{transition}",
		Description: "State transitions the consumer committed, by entity and normalized states.",
		Labels:      []string{LabelEntity, LabelFrom, LabelTo}},

	{Name: OutboxPublishDuration, Kind: KindHistogram, Unit: "s",
		Description: "Time the broker took to confirm or refuse one outbox message.",
		Labels:      []string{LabelOutcome}},
	{Name: OutboxPublishFailures, Kind: KindCounter, Unit: "{message}",
		Description: "Outbox publications that did not end confirmed, by normalized reason.",
		Labels:      []string{LabelReason}},
	{Name: OutboxRelayCycleTime, Kind: KindHistogram, Unit: "s",
		Description: "Duration of one relay cycle: lease, publish and settle a batch.",
		Labels:      []string{LabelOutcome}},
	{Name: OutboxRelayCycleFailure, Kind: KindCounter, Unit: "{cycle}",
		Description: "Relay cycles that failed, by stage.",
		Labels:      []string{LabelStage}},
	{Name: OutboxRelayStuck, Kind: KindCounter, Unit: "{message}",
		Description: "Outbox messages reported as stuck after the configured attempt threshold."},
	{Name: OutboxRelayAbandoned, Kind: KindCounter, Unit: "{message}",
		Description: "Outbox messages abandoned after a permanent failure."},
	{Name: OutboxRelayLeaseLost, Kind: KindCounter, Unit: "{message}",
		Description: "Outbox outcomes that could not be recorded because the lease had expired."},

	{Name: ConsumerHandleDuration, Kind: KindHistogram, Unit: "s",
		Description: "Time to apply one delivered event, including recording a failure.",
		Labels:      []string{LabelDisposition}},
	{Name: ConsumerNoOps, Kind: KindCounter, Unit: "{event}",
		Description: "Delivered events that changed nothing, by reason.",
		Labels:      []string{LabelReason}},
	{Name: ConsumerFailures, Kind: KindCounter, Unit: "{event}",
		Description: "Delivered events whose handling failed, by normalized reason.",
		Labels:      []string{LabelReason}},
	{Name: ConsumerRetries, Kind: KindCounter, Unit: "{event}",
		Description: "Delivered events scheduled for another attempt."},
	{Name: ConsumerDeadLetters, Kind: KindCounter, Unit: "{event}",
		Description: "Delivered events sent to the dead-letter queue, by normalized reason.",
		Labels:      []string{LabelReason}},

	{Name: BrokerRedeliveries, Kind: KindCounter, Unit: "{message}",
		Description: "Deliveries the broker flagged as redelivered."},
	{Name: BrokerRepublished, Kind: KindCounter, Unit: "{message}",
		Description: "Messages republished to a retry tier or the dead letter, with the copy confirmed.",
		Labels:      []string{LabelDestination, LabelQueue}},
	{Name: BrokerRejected, Kind: KindCounter, Unit: "{message}",
		Description: "Messages whose body could not be read and were dead-lettered directly."},
	{Name: BrokerUnacknowledged, Kind: KindCounter, Unit: "{message}",
		Description: "Messages left unacknowledged for the broker to redeliver."},

	{Name: OutboxPending, Kind: KindGauge, Unit: "{message}",
		Description: "Outbox messages not yet published: pending, plus leased ones whose outcome is not recorded."},
	{Name: OutboxOldestAge, Kind: KindGauge, Unit: "s",
		Description: "Age of the oldest unpublished outbox message."},
	{Name: InboxPending, Kind: KindGauge, Unit: "{event}",
		Description: "Inbox events not yet applied: pending, plus ones being processed."},
	{Name: InboxFailed, Kind: KindGauge, Unit: "{event}",
		Description: "Inbox events marked failed and waiting for an operator."},
	{Name: InboxOldestAge, Kind: KindGauge, Unit: "s",
		Description: "Age of the oldest inbox event not yet applied."},
	{Name: QueueDepth, Kind: KindGauge, Unit: "{message}",
		Description: "Messages ready in each known queue, as last sampled from the broker.",
		Labels:      []string{LabelQueue}},

	{Name: PoolConnections, Kind: KindGauge, Unit: "{connection}",
		Description: "PostgreSQL pool connections, by state.",
		Labels:      []string{LabelState}},
	{Name: PoolMaxOpen, Kind: KindGauge, Unit: "{connection}",
		Description: "Configured ceiling of open PostgreSQL connections."},
	{Name: PoolWaits, Kind: KindObservableCounter, Unit: "{wait}",
		Description: "Times a caller waited for a PostgreSQL connection."},

	{Name: SamplerAge, Kind: KindGauge, Unit: "s",
		Description: "Seconds since a background sampler last collected successfully.",
		Labels:      []string{LabelSampler}},
	{Name: SamplerFailures, Kind: KindObservableCounter, Unit: "{sample}",
		Description: "Background sample attempts that failed or timed out.",
		Labels:      []string{LabelSampler}},

	{Name: HTTPRateLimited, Kind: KindCounter, Unit: "{request}",
		Description: "Requests refused with 429 by a rate limiter, by limiter and route class.",
		Labels:      []string{LabelLimiter, LabelRouteClass}},
}

// Catalogue returns a copy of every instrument this package creates.
func Catalogue() []Instrument {
	copied := make([]Instrument, len(catalogue))
	for index, instrument := range catalogue {
		copied[index] = instrument
		copied[index].Labels = append([]string(nil), instrument.Labels...)
	}
	return copied
}

func lookup(name string) (Instrument, bool) {
	for _, instrument := range catalogue {
		if instrument.Name == name {
			return instrument, true
		}
	}
	return Instrument{}, false
}
