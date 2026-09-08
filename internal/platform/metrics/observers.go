package metrics

import (
	"context"
	"errors"

	consumerapp "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	outboxapp "github.com/rmotti/payments-boilerplate/internal/application/outbox"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
)

// The observers below are the whole reason this package exists. Each one
// implements an interface an application or adapter package already declared,
// in that package's own vocabulary, and translates it into instruments. That
// is what keeps OpenTelemetry out of the domain and out of the use cases.
//
// They are called from the request and processing paths, so they never block:
// every method records and returns. Nothing here reads a database or a broker.

// WebhookObserver records how provider events are received.
type WebhookObserver struct{ metrics *Metrics }

// NewWebhookObserver binds the receiving use case to its instruments.
func NewWebhookObserver(metrics *Metrics) WebhookObserver {
	return WebhookObserver{metrics: metrics}
}

// Received records one receive attempt.
func (o WebhookObserver) Received(receipt webhookapp.Receipt) {
	ctx := context.Background()
	provider := Attr(LabelProvider, string(receipt.Provider))
	// KindUnknown represents every provider event the application deliberately
	// ignores. It is outside the allowlist and therefore becomes Other, just
	// like any future provider kind that has not been reviewed yet.
	kind := Attr(LabelEventKind, string(receipt.Kind))

	if receipt.Err != nil {
		reason := receiveFailureReason(receipt.Err)
		o.metrics.add(ctx, WebhookReceiveFailures, 1, provider, Attr(LabelReason, reason))
		if reason == "invalid_signature" {
			o.metrics.add(ctx, WebhookInvalidSignature, 1, provider)
		}
		o.metrics.record(ctx, WebhookReceiveDuration, receipt.Duration.Seconds(),
			provider, Attr(LabelOutcome, "error"))
		return
	}

	outcome := Attr(LabelOutcome, string(receipt.Outcome))
	o.metrics.add(ctx, WebhookReceived, 1, provider, kind, outcome)
	o.metrics.record(ctx, WebhookReceiveDuration, receipt.Duration.Seconds(), provider, outcome)
	if receipt.Outcome == webhookapp.OutcomeDuplicate {
		o.metrics.add(ctx, WebhookDuplicate, 1, provider, kind)
	}
}

// receiveFailureReason classifies a receive failure without ever using the
// error text. Only the classification reaches the label.
func receiveFailureReason(err error) string {
	switch {
	case errors.Is(err, webhookapp.ErrInvalidSignature):
		return "invalid_signature"
	case errors.Is(err, webhookapp.ErrPayloadTooLarge):
		return "storage"
	default:
		return "internal"
	}
}

// CheckoutObserver records provider latency and idempotency outcomes of the
// checkout use case.
type CheckoutObserver struct{ metrics *Metrics }

// NewCheckoutObserver binds the checkout use case to its instruments.
func NewCheckoutObserver(metrics *Metrics) CheckoutObserver {
	return CheckoutObserver{metrics: metrics}
}

// ProviderCalled records one provider request and how it ended.
func (o CheckoutObserver) ProviderCalled(call paymentapp.ProviderCall) {
	ctx := context.Background()
	provider := Attr(LabelProvider, string(call.Provider))
	operation := Attr(LabelOperation, string(call.Operation))
	outcome := "success"
	if call.Err != nil {
		outcome = "error"
	}
	normalized := Attr(LabelOutcome, outcome)
	o.metrics.record(ctx, ProviderRequestDuration, call.Duration.Seconds(), provider, operation, normalized)
	if call.Err != nil {
		o.metrics.add(ctx, ProviderRequestFailures, 1, provider, operation, normalized)
	}
}

// Replayed records a checkout answered from an earlier attempt.
func (o CheckoutObserver) Replayed() {
	o.metrics.add(context.Background(), IdempotencyReplays, 1,
		Attr(LabelOperation, string(paymentapp.ProviderOperationCreateCheckout)))
}

// Conflicted records an idempotency key reused with a different order.
func (o CheckoutObserver) Conflicted() {
	o.metrics.add(context.Background(), IdempotencyConflicts, 1,
		Attr(LabelOperation, string(paymentapp.ProviderOperationCreateCheckout)))
}

// OrderObserver records idempotency outcomes of order creation.
type OrderObserver struct{ metrics *Metrics }

// NewOrderObserver binds the order use case to its instruments.
func NewOrderObserver(metrics *Metrics) OrderObserver { return OrderObserver{metrics: metrics} }

// Replayed records an order returned from an earlier equivalent request.
func (o OrderObserver) Replayed() {
	o.metrics.add(context.Background(), IdempotencyReplays, 1, Attr(LabelOperation, "create_order"))
}

// Conflicted records an idempotency key reused with a different request.
func (o OrderObserver) Conflicted() {
	o.metrics.add(context.Background(), IdempotencyConflicts, 1, Attr(LabelOperation, "create_order"))
}

var (
	_ webhookapp.Observer = WebhookObserver{}
	_ paymentapp.Observer = CheckoutObserver{}
	_ orderapp.Observer   = OrderObserver{}
)

// RelayObserver records what the outbox relay does.
type RelayObserver struct{ metrics *Metrics }

// NewRelayObserver binds the relay to its instruments.
func NewRelayObserver(metrics *Metrics) RelayObserver { return RelayObserver{metrics: metrics} }

// Stuck records a message that keeps failing past the alert threshold.
func (o RelayObserver) Stuck(string, int, error) {
	o.metrics.add(context.Background(), OutboxRelayStuck, 1)
}

// Abandoned records a message a permanent failure stopped.
func (o RelayObserver) Abandoned(string, int, error) {
	o.metrics.add(context.Background(), OutboxRelayAbandoned, 1)
}

// LeaseLost records an outcome that could not be recorded in time.
func (o RelayObserver) LeaseLost(string, error) {
	o.metrics.add(context.Background(), OutboxRelayLeaseLost, 1)
}

// PublicationSettled records one publish attempt and its settled outcome.
func (o RelayObserver) PublicationSettled(publication outboxapp.Publication) {
	ctx := context.Background()
	outcome := Attr(LabelOutcome, publication.Outcome.String())
	o.metrics.record(ctx, OutboxPublishDuration, publication.Duration.Seconds(), outcome)
	if publication.Outcome == outboxapp.OutcomePublished {
		return
	}
	o.metrics.add(ctx, OutboxPublishFailures, 1,
		Attr(LabelReason, publishFailureReason(publication.Outcome, publication.Cause)))
}

// CycleCompleted records one relay cycle.
func (o RelayObserver) CycleCompleted(cycle outboxapp.Cycle) {
	ctx := context.Background()
	o.metrics.record(ctx, OutboxRelayCycleTime, cycle.Duration.Seconds(),
		Attr(LabelOutcome, cycleOutcome(cycle)))
	if cycle.Err != nil {
		o.metrics.add(ctx, OutboxRelayCycleFailure, 1, Attr(LabelStage, cycle.Stage.String()))
	}
}

// cycleOutcome distinguishes a cycle that failed, one that found nothing and
// one that did work. The counts themselves stay out of the label.
func cycleOutcome(cycle outboxapp.Cycle) string {
	switch {
	case cycle.Err != nil:
		return "failed"
	case cycle.Result.Empty():
		return "empty"
	default:
		return "published"
	}
}

// publishFailureReason classifies a publication failure. The error text never
// becomes a label: a broker error can carry a URL with credentials.
func publishFailureReason(outcome outboxapp.Outcome, cause error) string {
	switch {
	case outcome == outboxapp.OutcomeLeaseLost:
		return "lease_lost"
	case errors.Is(cause, outboxapp.ErrNotRouted):
		return "not_routed"
	case errors.Is(cause, outboxapp.ErrNotConfirmed):
		return "not_confirmed"
	case outboxapp.IsPermanent(cause):
		return "permanent"
	default:
		return "transient"
	}
}

var (
	_ outboxapp.Observer      = RelayObserver{}
	_ outboxapp.CycleObserver = RelayObserver{}
)

// ConsumerObserver records what applying an event produced.
type ConsumerObserver struct{ metrics *Metrics }

// NewConsumerObserver binds the consuming use case to its instruments.
func NewConsumerObserver(metrics *Metrics) ConsumerObserver {
	return ConsumerObserver{metrics: metrics}
}

// Applied records the transitions one event committed.
func (o ConsumerObserver) Applied(_ string, effect consumerapp.Effect) {
	// The transitions themselves are recorded from the Report, which carries
	// the state each entity moved from. This method exists to satisfy the
	// existing Observer interface.
	_ = effect
}

// NoOp satisfies the existing Observer interface; the Report carries the
// classification the counter needs.
func (o ConsumerObserver) NoOp(string, string) {}

// Retrying records an event scheduled for another attempt.
func (o ConsumerObserver) Retrying(string, int, error) {
	o.metrics.add(context.Background(), ConsumerRetries, 1)
}

// DeadLettered records an event that will not be tried again.
func (o ConsumerObserver) DeadLettered(_ string, _ int, cause error) {
	o.metrics.add(context.Background(), ConsumerDeadLetters, 1,
		Attr(LabelReason, consumerFailureReason(cause)))
}

// Handled records the duration, the disposition and the committed transitions
// of one message.
func (o ConsumerObserver) Handled(_ string, report consumerapp.Report) {
	ctx := context.Background()
	disposition := "unrecorded"
	if report.Err == nil {
		disposition = report.Disposition.String()
	}
	o.metrics.record(ctx, ConsumerHandleDuration, report.Duration.Seconds(),
		Attr(LabelDisposition, disposition))

	if report.Err != nil {
		o.metrics.add(ctx, ConsumerFailures, 1, Attr(LabelReason, "storage"))
		return
	}
	if report.Disposition != consumerapp.DispositionDone {
		o.metrics.add(ctx, ConsumerFailures, 1, Attr(LabelReason, consumerFailureReason(report.Cause)))
		return
	}
	if !report.Applied {
		reason := "stale_event"
		if report.AlreadyProcessed {
			reason = "already_processed"
		}
		o.metrics.add(ctx, ConsumerNoOps, 1, Attr(LabelReason, reason))
		return
	}
	for _, transition := range report.Transitions {
		o.metrics.add(ctx, StateTransitions, 1,
			Attr(LabelEntity, string(transition.Entity)),
			Attr(LabelFrom, transition.From),
			Attr(LabelTo, transition.To))
	}
}

// consumerFailureReason maps a classified consumer error onto the allowlist.
// The error text is never used, only the sentinel it wraps.
func consumerFailureReason(cause error) string {
	switch {
	case cause == nil:
		return "unclassified"
	case errors.Is(cause, consumerapp.ErrIncompatibleAPIVersion):
		return "incompatible_api_version"
	case errors.Is(cause, consumerapp.ErrUnreadablePayload):
		return "unreadable_payload"
	case errors.Is(cause, consumerapp.ErrMissingReference):
		return "missing_reference"
	case errors.Is(cause, consumerapp.ErrAggregateNotFound):
		return "aggregate_not_found"
	case errors.Is(cause, consumerapp.ErrReferenceMismatch):
		return "reference_mismatch"
	case errors.Is(cause, consumerapp.ErrAmountMismatch):
		return "amount_mismatch"
	case errors.Is(cause, consumerapp.ErrStateConflict):
		return "state_conflict"
	case errors.Is(cause, consumerapp.ErrInvariantViolated):
		return "invariant_violated"
	default:
		return "unclassified"
	}
}

var (
	_ consumerapp.Observer         = ConsumerObserver{}
	_ consumerapp.HandlingObserver = ConsumerObserver{}
)

// RateLimitObserver records requests the HTTP boundary refused with 429.
//
// It takes two already-classified strings rather than a request, which is what
// keeps the identity out of the metric: the transport layer knows the client
// address and the credential fingerprint, and neither reaches this package.
// What is recorded is which limiter refused and which class of route it
// guards, both closed vocabularies, so the series count is fixed no matter how
// many callers or paths the deployment has.
type RateLimitObserver struct{ metrics *Metrics }

// NewRateLimitObserver binds the HTTP rate limiters to their instrument.
func NewRateLimitObserver(metrics *Metrics) RateLimitObserver {
	return RateLimitObserver{metrics: metrics}
}

// Rejected records one refused request.
func (o RateLimitObserver) Rejected(limiter, routeClass string) {
	if o.metrics == nil {
		return
	}
	o.metrics.add(context.Background(), HTTPRateLimited, 1,
		Attr(LabelLimiter, limiter),
		Attr(LabelRouteClass, routeClass))
}
