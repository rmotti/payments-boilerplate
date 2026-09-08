package metrics

import (
	"context"
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
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// reader builds instruments against an in-memory MeterProvider, which is what
// lets a test assert the exported name, description, unit, type and attributes
// rather than only that a method was called.
func reader(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	manual := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(manual))
	built, err := NewWithProvider(provider)
	if err != nil {
		t.Fatalf("NewWithProvider() error = %v", err)
	}
	return built, manual
}

// collected reads every metric the reader holds, keyed by instrument name.
func collected(t *testing.T, manual *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var resource metricdata.ResourceMetrics
	if err := manual.Collect(context.Background(), &resource); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	found := make(map[string]metricdata.Metrics)
	for _, scoped := range resource.ScopeMetrics {
		for _, metric := range scoped.Metrics {
			found[metric.Name] = metric
		}
	}
	return found
}

// sumFor returns the value of one counter data point matching want.
func sumFor(t *testing.T, metric metricdata.Metrics, want ...attribute.KeyValue) int64 {
	t.Helper()
	sum, ok := metric.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 sum", metric.Name, metric.Data)
	}
	for _, point := range sum.DataPoints {
		if matches(point.Attributes, want) {
			return point.Value
		}
	}
	t.Fatalf("%s has no data point with %v; points=%s", metric.Name, want, describe(sum.DataPoints))
	return 0
}

func histogramCount(t *testing.T, metric metricdata.Metrics, want ...attribute.KeyValue) uint64 {
	t.Helper()
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s is %T, want a float64 histogram", metric.Name, metric.Data)
	}
	for _, point := range histogram.DataPoints {
		if matches(point.Attributes, want) {
			return point.Count
		}
	}
	t.Fatalf("%s has no data point with %v", metric.Name, want)
	return 0
}

func gaugeValue(t *testing.T, metric metricdata.Metrics, want ...attribute.KeyValue) float64 {
	t.Helper()
	switch data := metric.Data.(type) {
	case metricdata.Gauge[float64]:
		for _, point := range data.DataPoints {
			if matches(point.Attributes, want) {
				return point.Value
			}
		}
	case metricdata.Gauge[int64]:
		for _, point := range data.DataPoints {
			if matches(point.Attributes, want) {
				return float64(point.Value)
			}
		}
	default:
		t.Fatalf("%s is %T, want a gauge", metric.Name, metric.Data)
	}
	t.Fatalf("%s has no data point with %v", metric.Name, want)
	return 0
}

func matches(got attribute.Set, want []attribute.KeyValue) bool {
	for _, attr := range want {
		value, ok := got.Value(attr.Key)
		if !ok || value != attr.Value {
			return false
		}
	}
	return true
}

func describe[T any](points []T) string {
	parts := make([]string, 0, len(points))
	for _, point := range points {
		parts = append(parts, fmt.Sprintf("%v", point))
	}
	return strings.Join(parts, ", ")
}

func TestEveryInstrumentMatchesItsCatalogueEntry(t *testing.T) {
	t.Parallel()
	built, manual := reader(t)

	// Every synchronous instrument is recorded once so the reader exports it.
	// The observable ones are registered by the samplers and the pool
	// collector, which have their own tests.
	exercise(built)
	found := collected(t, manual)

	for _, instrument := range Catalogue() {
		if instrument.Kind == KindObservableCounter {
			continue
		}
		metric, ok := found[instrument.Name]
		if !ok {
			t.Errorf("instrument %s was not exported", instrument.Name)
			continue
		}
		if metric.Description != instrument.Description {
			t.Errorf("%s description = %q, want %q", instrument.Name, metric.Description, instrument.Description)
		}
		if metric.Unit != instrument.Unit {
			t.Errorf("%s unit = %q, want %q", instrument.Name, metric.Unit, instrument.Unit)
		}
		if got := kindOf(metric); got != instrument.Kind {
			t.Errorf("%s kind = %s, want %s", instrument.Name, got, instrument.Kind)
		}
	}
}

func kindOf(metric metricdata.Metrics) Kind {
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		if data.IsMonotonic {
			return KindCounter
		}
		return KindGauge
	case metricdata.Histogram[float64]:
		return KindHistogram
	case metricdata.Gauge[float64], metricdata.Gauge[int64]:
		return KindGauge
	default:
		return Kind(fmt.Sprintf("%T", metric.Data))
	}
}

// exercise records one measurement on every synchronous instrument, through
// the observers rather than by touching the instruments directly. What the
// test asserts is therefore reachable from the application's own vocabulary.
func exercise(built *Metrics) {
	webhooks := NewWebhookObserver(built)
	webhooks.Received(webhookapp.Receipt{
		Provider: webhookdomain.Stripe, Kind: webhookdomain.KindCheckoutCompleted,
		Outcome: webhookapp.OutcomeAccepted, Duration: 10 * time.Millisecond,
	})
	webhooks.Received(webhookapp.Receipt{
		Provider: webhookdomain.Stripe, Kind: webhookdomain.KindCheckoutCompleted,
		Outcome: webhookapp.OutcomeDuplicate, Duration: time.Millisecond,
	})
	webhooks.Received(webhookapp.Receipt{
		Provider: webhookdomain.Stripe, Duration: time.Millisecond,
		Err: fmt.Errorf("%w: bad mac", webhookapp.ErrInvalidSignature),
	})

	checkout := NewCheckoutObserver(built)
	checkout.ProviderCalled(paymentapp.ProviderCall{
		Provider: paymentdomain.Stripe, Operation: paymentapp.ProviderOperationCreateCheckout,
		Duration: 20 * time.Millisecond,
	})
	checkout.ProviderCalled(paymentapp.ProviderCall{
		Provider: paymentdomain.Stripe, Operation: paymentapp.ProviderOperationCreateCheckout,
		Duration: 20 * time.Millisecond, Err: errors.New("provider down"),
	})
	checkout.Replayed()
	checkout.Conflicted()

	relay := NewRelayObserver(built)
	relay.Stuck("msg", 10, errors.New("nope"))
	relay.Abandoned("msg", 1, errors.New("nope"))
	relay.LeaseLost("msg", outboxapp.ErrLeaseLost)
	relay.PublicationSettled(outboxapp.Publication{Outcome: outboxapp.OutcomePublished, Duration: time.Millisecond})
	relay.PublicationSettled(outboxapp.Publication{
		Outcome: outboxapp.OutcomeRetrying, Duration: time.Millisecond, Cause: outboxapp.ErrNotConfirmed,
	})
	relay.CycleCompleted(outboxapp.Cycle{Result: outboxapp.Result{Leased: 1, Published: 1}, Duration: time.Millisecond})
	relay.CycleCompleted(outboxapp.Cycle{
		Duration: time.Millisecond, Err: errors.New("lease failed"), Stage: outboxapp.StageLease,
	})

	consumer := NewConsumerObserver(built)
	consumer.Handled("evt", consumerapp.Report{
		Disposition: consumerapp.DispositionDone, Applied: true, Duration: time.Millisecond,
		Transitions: []consumerapp.Transition{{Entity: consumerapp.EntityPayment, From: "pending", To: "succeeded"}},
	})
	consumer.Handled("evt", consumerapp.Report{
		Disposition: consumerapp.DispositionDone, Duration: time.Millisecond, AlreadyProcessed: true,
	})
	consumer.Handled("evt", consumerapp.Report{
		Disposition: consumerapp.DispositionRetry, Duration: time.Millisecond, Cause: errors.New("database blinked"),
	})
	consumer.Retrying("evt", 1, errors.New("database blinked"))
	consumer.DeadLettered("evt", 2, consumerapp.ErrAmountMismatch)

	broker := NewBrokerObserver(built, []string{"payments.webhooks.dlq"})
	broker.Redelivered("msg")
	broker.Rejected("msg", errors.New("unreadable"))
	broker.Republished("msg", "payments.webhooks.dlq", 2)
	broker.Failed("msg", errors.New("unrecorded"))

	orders := NewOrderObserver(built)
	orders.Replayed()
	orders.Conflicted()

	// The gauges the samplers own are recorded directly here so the catalogue
	// check covers them too; their real path has its own test.
	ctx := context.Background()
	built.set(ctx, OutboxPending, 1)
	built.set(ctx, OutboxOldestAge, 1)
	built.set(ctx, InboxPending, 1)
	built.set(ctx, InboxFailed, 1)
	built.set(ctx, InboxOldestAge, 1)
	built.set(ctx, QueueDepth, 1, attribute.String(LabelQueue, "payments.webhooks"))
	built.set(ctx, PoolConnections, 1, Attr(LabelState, "idle"))
	built.set(ctx, PoolMaxOpen, 1)
	built.set(ctx, SamplerAge, 1, Attr(LabelSampler, SamplerBacklog))
}
