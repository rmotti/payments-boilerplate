package metrics

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	consumerapp "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	outboxapp "github.com/rmotti/payments-boilerplate/internal/application/outbox"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	paymentdomain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	webhookdomain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestWebhookObserverSeparatesAcceptedDuplicateAndInvalid(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	observer := NewWebhookObserver(built)

	observer.Received(webhookapp.Receipt{
		Provider: webhookdomain.Stripe, Kind: webhookdomain.KindCheckoutCompleted,
		Outcome: webhookapp.OutcomeAccepted, Duration: 5 * time.Millisecond,
	})
	observer.Received(webhookapp.Receipt{
		Provider: webhookdomain.Stripe, Kind: webhookdomain.KindCheckoutCompleted,
		Outcome: webhookapp.OutcomeDuplicate, Duration: time.Millisecond,
	})
	observer.Received(webhookapp.Receipt{
		Provider: webhookdomain.Stripe, Duration: time.Millisecond,
		Err: fmt.Errorf("%w: signature mismatch for whsec_live_supersecret", webhookapp.ErrInvalidSignature),
	})

	found := collected(t, manual)
	provider := attribute.String(LabelProvider, "stripe")
	if got := sumFor(t, found[WebhookReceived], provider, attribute.String(LabelOutcome, "accepted")); got != 1 {
		t.Errorf("accepted events = %d, want 1", got)
	}
	if got := sumFor(t, found[WebhookDuplicate], provider); got != 1 {
		t.Errorf("duplicates = %d, want 1", got)
	}
	if got := sumFor(t, found[WebhookInvalidSignature], provider); got != 1 {
		t.Errorf("invalid signatures = %d, want 1", got)
	}
	if got := sumFor(t, found[WebhookReceiveFailures], provider,
		attribute.String(LabelReason, "invalid_signature")); got != 1 {
		t.Errorf("receive failures = %d, want 1", got)
	}
	if got := histogramCount(t, found[WebhookReceiveDuration], provider,
		attribute.String(LabelOutcome, "error")); got != 1 {
		t.Errorf("error durations = %d, want 1", got)
	}
}

// An unhandled provider event type must not become a label of its own: the
// application does not enumerate every Stripe event, so an event it ignores
// would otherwise add a series each time Stripe adds a type.
func TestWebhookObserverCollapsesUnhandledEventTypes(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)

	NewWebhookObserver(built).Received(webhookapp.Receipt{
		Provider: webhookdomain.Stripe, Kind: webhookdomain.KindUnknown,
		Outcome: webhookapp.OutcomeIgnored, Duration: time.Millisecond,
	})

	found := collected(t, manual)
	if got := sumFor(t, found[WebhookReceived],
		attribute.String(LabelEventKind, Other),
		attribute.String(LabelOutcome, "ignored")); got != 1 {
		t.Errorf("ignored events = %d, want 1", got)
	}
}

func TestCheckoutObserverRecordsSuccessAndFailureSeparately(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	observer := NewCheckoutObserver(built)

	observer.ProviderCalled(paymentapp.ProviderCall{
		Provider: paymentdomain.Stripe, Operation: paymentapp.ProviderOperationCreateCheckout,
		Duration: 30 * time.Millisecond,
	})
	observer.ProviderCalled(paymentapp.ProviderCall{
		Provider: paymentdomain.Stripe, Operation: paymentapp.ProviderOperationCreateCheckout,
		Duration: 40 * time.Millisecond, Err: errors.New("sk_test_secret rejected by provider"),
	})

	found := collected(t, manual)
	operation := attribute.String(LabelOperation, "create_checkout")
	if got := histogramCount(t, found[ProviderRequestDuration], operation,
		attribute.String(LabelOutcome, "success")); got != 1 {
		t.Errorf("successful provider calls = %d, want 1", got)
	}
	if got := sumFor(t, found[ProviderRequestFailures], operation,
		attribute.String(LabelOutcome, "error")); got != 1 {
		t.Errorf("provider failures = %d, want 1", got)
	}
}

func TestIdempotencyObserversSeparateOperations(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)

	NewOrderObserver(built).Replayed()
	NewOrderObserver(built).Conflicted()
	NewCheckoutObserver(built).Replayed()

	found := collected(t, manual)
	if got := sumFor(t, found[IdempotencyReplays], attribute.String(LabelOperation, "create_order")); got != 1 {
		t.Errorf("order replays = %d, want 1", got)
	}
	if got := sumFor(t, found[IdempotencyReplays], attribute.String(LabelOperation, "create_checkout")); got != 1 {
		t.Errorf("checkout replays = %d, want 1", got)
	}
	if got := sumFor(t, found[IdempotencyConflicts], attribute.String(LabelOperation, "create_order")); got != 1 {
		t.Errorf("order conflicts = %d, want 1", got)
	}
}

func TestRelayObserverClassifiesPublicationOutcomes(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	observer := NewRelayObserver(built)

	observer.PublicationSettled(outboxapp.Publication{
		Outcome: outboxapp.OutcomePublished, Duration: 2 * time.Millisecond,
	})
	observer.PublicationSettled(outboxapp.Publication{
		Outcome: outboxapp.OutcomeRetrying, Duration: time.Millisecond,
		Cause: fmt.Errorf("%w: amqp://payments:hunter2@broker:5672", outboxapp.ErrNotRouted),
	})
	observer.PublicationSettled(outboxapp.Publication{
		Outcome: outboxapp.OutcomeFailed, Duration: time.Millisecond,
		Cause: outboxapp.Permanent(errors.New("cannot encode")),
	})
	observer.PublicationSettled(outboxapp.Publication{
		Outcome: outboxapp.OutcomeLeaseLost, Duration: time.Millisecond, Cause: outboxapp.ErrLeaseLost,
	})

	found := collected(t, manual)
	for reason, want := range map[string]int64{"not_routed": 1, "permanent": 1, "lease_lost": 1} {
		if got := sumFor(t, found[OutboxPublishFailures], attribute.String(LabelReason, reason)); got != want {
			t.Errorf("publish failures[%s] = %d, want %d", reason, got, want)
		}
	}
	if got := histogramCount(t, found[OutboxPublishDuration],
		attribute.String(LabelOutcome, "published")); got != 1 {
		t.Errorf("published durations = %d, want 1", got)
	}
}

func TestRelayObserverSeparatesEmptyAndFailedCycles(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	observer := NewRelayObserver(built)

	observer.CycleCompleted(outboxapp.Cycle{Duration: time.Millisecond})
	observer.CycleCompleted(outboxapp.Cycle{
		Result: outboxapp.Result{Leased: 2, Published: 2}, Duration: 3 * time.Millisecond,
	})
	observer.CycleCompleted(outboxapp.Cycle{
		Duration: time.Millisecond, Stage: outboxapp.StageSettlement,
		Err: errors.New("mark published: connection refused on postgres://payments:hunter2@db"),
	})

	found := collected(t, manual)
	if got := histogramCount(t, found[OutboxRelayCycleTime], attribute.String(LabelOutcome, "empty")); got != 1 {
		t.Errorf("empty cycles = %d, want 1", got)
	}
	if got := sumFor(t, found[OutboxRelayCycleFailure], attribute.String(LabelStage, "settlement")); got != 1 {
		t.Errorf("settlement failures = %d, want 1", got)
	}
}

func TestConsumerObserverDistinguishesAppliedNoOpAndFailure(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	observer := NewConsumerObserver(built)

	observer.Handled("evt_1", consumerapp.Report{
		Disposition: consumerapp.DispositionDone, Applied: true, Duration: 2 * time.Millisecond,
		Transitions: []consumerapp.Transition{
			{Entity: consumerapp.EntityAttempt, From: "pending", To: "succeeded"},
			{Entity: consumerapp.EntityPayment, From: "processing", To: "succeeded"},
			{Entity: consumerapp.EntityOrder, From: "pending", To: "paid"},
		},
	})
	observer.Handled("evt_2", consumerapp.Report{
		Disposition: consumerapp.DispositionDone, Duration: time.Millisecond, AlreadyProcessed: true,
	})
	observer.Handled("evt_3", consumerapp.Report{
		Disposition: consumerapp.DispositionDone, Duration: time.Millisecond,
	})
	observer.Handled("evt_4", consumerapp.Report{
		Disposition: consumerapp.DispositionDead, Duration: time.Millisecond,
		Cause: fmt.Errorf("%w: provider settled 200 BRL", consumerapp.ErrAmountMismatch),
	})
	observer.Handled("evt_5", consumerapp.Report{
		Duration: time.Millisecond, Err: errors.New("record consumer failure"),
	})

	found := collected(t, manual)
	if got := sumFor(t, found[StateTransitions],
		attribute.String(LabelEntity, "order"),
		attribute.String(LabelFrom, "pending"),
		attribute.String(LabelTo, "paid")); got != 1 {
		t.Errorf("order transitions = %d, want 1", got)
	}
	if got := sumFor(t, found[ConsumerNoOps], attribute.String(LabelReason, "already_processed")); got != 1 {
		t.Errorf("redelivery no-ops = %d, want 1", got)
	}
	if got := sumFor(t, found[ConsumerNoOps], attribute.String(LabelReason, "stale_event")); got != 1 {
		t.Errorf("stale no-ops = %d, want 1", got)
	}
	if got := sumFor(t, found[ConsumerFailures], attribute.String(LabelReason, "amount_mismatch")); got != 1 {
		t.Errorf("amount mismatches = %d, want 1", got)
	}
	if got := sumFor(t, found[ConsumerFailures], attribute.String(LabelReason, "storage")); got != 1 {
		t.Errorf("unrecorded failures = %d, want 1", got)
	}
	if got := histogramCount(t, found[ConsumerHandleDuration],
		attribute.String(LabelDisposition, "unrecorded")); got != 1 {
		t.Errorf("unrecorded durations = %d, want 1", got)
	}
}

// A queue name outside the declared topology must not create a series. A
// renamed queue or an unexpected destination is a bug to notice, not a new
// dimension to carry.
func TestBrokerObserverBoundsQueueNamesToTheTopology(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	observer := NewBrokerObserver(built, []string{"payments.webhooks.retry.5s", "payments.webhooks.dlq"})

	observer.Republished("msg_1", "payments.webhooks.retry.5s", 1)
	observer.Republished("msg_2", "payments.webhooks.dlq", 3)
	observer.Republished("msg_3", "queue.someone.renamed", 1)
	observer.Redelivered("msg_4")

	found := collected(t, manual)
	if got := sumFor(t, found[BrokerRepublished],
		attribute.String(LabelDestination, "retry"),
		attribute.String(LabelQueue, "payments.webhooks.retry.5s")); got != 1 {
		t.Errorf("retry republications = %d, want 1", got)
	}
	if got := sumFor(t, found[BrokerRepublished],
		attribute.String(LabelDestination, "dead_letter"),
		attribute.String(LabelQueue, "payments.webhooks.dlq")); got != 1 {
		t.Errorf("dead letters = %d, want 1", got)
	}
	if got := sumFor(t, found[BrokerRepublished], attribute.String(LabelQueue, Other)); got != 1 {
		t.Errorf("unknown queue republications = %d, want 1", got)
	}
	if got := sumFor(t, found[BrokerRedeliveries]); got != 1 {
		t.Errorf("redeliveries = %d, want 1", got)
	}
}

// The allowlist is the guarantee, so this asserts it over everything the
// observers can emit rather than over a hand-written list of expectations.
func TestNoInstrumentCarriesAnUnexpectedLabelOrValue(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	exercise(built)
	exerciseWithHostileInput(built)

	described := make(map[string][]string, len(catalogue))
	for _, instrument := range Catalogue() {
		described[instrument.Name] = instrument.Labels
	}

	for name, metric := range collected(t, manual) {
		declared, known := described[name]
		if !known {
			t.Errorf("instrument %s is exported but not catalogued", name)
			continue
		}
		for _, set := range attributeSets(t, metric) {
			for _, attr := range set.ToSlice() {
				key := string(attr.Key)
				if !contains(declared, key) {
					t.Errorf("%s carries label %q, which its catalogue entry does not declare", name, key)
					continue
				}
				value := attr.Value.String()
				if key == LabelQueue {
					// Queue values come from the declared topology, checked by
					// the broker observer and the sampler.
					continue
				}
				if Normalize(key, value) != value {
					t.Errorf("%s carries %s=%q, which is outside the allowlist", name, key, value)
				}
			}
		}
	}
}

// exerciseWithHostileInput feeds the observers exactly what must never reach a
// label: identifiers, correlation ids, secrets and raw error text.
func exerciseWithHostileInput(built *Metrics) {
	NewWebhookObserver(built).Received(webhookapp.Receipt{
		Provider: webhookdomain.Provider("acme-" + secretish),
		Kind:     webhookdomain.Kind("customer.subscription.deleted"),
		Outcome:  webhookapp.Outcome("weird-" + secretish),
		Duration: time.Millisecond,
	})
	NewCheckoutObserver(built).ProviderCalled(paymentapp.ProviderCall{
		Provider: paymentdomain.Provider(secretish), Operation: paymentapp.ProviderOperation("refund-" + secretish),
		Duration: time.Millisecond, Err: errors.New(secretish),
	})
	NewConsumerObserver(built).Handled(secretish, consumerapp.Report{
		Disposition: consumerapp.DispositionDone, Applied: true, Duration: time.Millisecond,
		Transitions: []consumerapp.Transition{
			{Entity: consumerapp.Entity("customer"), From: secretish, To: "ord_" + secretish},
		},
	})
	NewBrokerObserver(built, nil).Republished(secretish, "queue."+secretish, 1)
	NewRelayObserver(built).PublicationSettled(outboxapp.Publication{
		Outcome: outboxapp.Outcome(99), Duration: time.Millisecond, Cause: errors.New(secretish),
	})
}

// secretish stands in for everything the ADR forbids as a label: an
// identifier, a correlation id and a credential in one string.
const secretish = "evt_1MxYz3Kq whsec_live_9f3a corr-8b21-postgres://payments:hunter2@db"

func TestNoRecordedLabelValueLeaksIdentifiersOrSecrets(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)
	exercise(built)
	exerciseWithHostileInput(built)

	forbidden := []string{"evt_", "whsec_", "sk_test", "corr-", "hunter2", "postgres://", "amqp://", "@db"}
	for name, metric := range collected(t, manual) {
		for _, set := range attributeSets(t, metric) {
			for _, attr := range set.ToSlice() {
				value := attr.Value.String()
				for _, needle := range forbidden {
					if strings.Contains(value, needle) {
						t.Errorf("%s label %s carries %q, which contains %q", name, attr.Key, value, needle)
					}
				}
			}
		}
	}
}

func attributeSets(t *testing.T, metric metricdata.Metrics) []attribute.Set {
	t.Helper()
	var sets []attribute.Set
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		for _, point := range data.DataPoints {
			sets = append(sets, point.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, point := range data.DataPoints {
			sets = append(sets, point.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, point := range data.DataPoints {
			sets = append(sets, point.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, point := range data.DataPoints {
			sets = append(sets, point.Attributes)
		}
	default:
		t.Fatalf("%s has unexpected data %T", metric.Name, metric.Data)
	}
	return sets
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// With OTLP disabled the binaries never install a MeterProvider, so every
// instrument is a no-op. Recording through them must stay safe and silent.
func TestObserversAreSafeWithoutAMeterProvider(t *testing.T) {
	t.Parallel()
	built := NewNoop()

	exercise(built)
	exerciseWithHostileInput(built)
	RegisterPoolMetricsMustNotFail(t, built)
}

// RegisterPoolMetricsMustNotFail keeps the no-op assertion readable.
func RegisterPoolMetricsMustNotFail(t *testing.T, built *Metrics) {
	t.Helper()
	if err := RegisterPoolMetrics(built, nil, nil); err != nil {
		t.Fatalf("RegisterPoolMetrics() with no database error = %v", err)
	}
}
