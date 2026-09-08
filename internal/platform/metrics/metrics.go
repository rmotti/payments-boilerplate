package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// scope names the instrumentation library the instruments belong to.
const scope = "github.com/rmotti/payments-boilerplate/internal/platform/metrics"

// Metrics owns every instrument the application records.
//
// It is safe to use with the global no-op MeterProvider: when OTLP export is
// disabled, telemetry never installs a real provider, every instrument here
// becomes a no-op and behaviour is unchanged. Nothing in the application
// branches on whether metrics are enabled.
type Metrics struct {
	meter metric.Meter

	counters   map[string]metric.Int64Counter
	histograms map[string]metric.Float64Histogram
	gauges     map[string]metric.Float64Gauge
}

// New builds every instrument of the catalogue from the global MeterProvider.
func New() (*Metrics, error) { return NewWithProvider(otel.GetMeterProvider()) }

// NewNoop returns instruments bound to a no-op provider. It exists for
// composition paths that want a non-nil *Metrics without recording anything.
func NewNoop() *Metrics {
	built, err := NewWithProvider(noop.NewMeterProvider())
	if err != nil {
		// The no-op provider cannot fail to create an instrument.
		panic("metrics: no-op provider failed: " + err.Error())
	}
	return built
}

// NewWithProvider builds every instrument of the catalogue from provider. A
// test passes an in-memory provider here; the binaries use New.
func NewWithProvider(provider metric.MeterProvider) (*Metrics, error) {
	meter := provider.Meter(scope)
	m := &Metrics{
		meter:      meter,
		counters:   make(map[string]metric.Int64Counter),
		histograms: make(map[string]metric.Float64Histogram),
		gauges:     make(map[string]metric.Float64Gauge),
	}
	for _, instrument := range catalogue {
		if err := m.create(instrument); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Metrics) create(instrument Instrument) error {
	switch instrument.Kind {
	case KindCounter:
		created, err := m.meter.Int64Counter(instrument.Name,
			metric.WithDescription(instrument.Description),
			metric.WithUnit(instrument.Unit))
		if err != nil {
			return fmt.Errorf("create counter %s: %w", instrument.Name, err)
		}
		m.counters[instrument.Name] = created
	case KindHistogram:
		created, err := m.meter.Float64Histogram(instrument.Name,
			metric.WithDescription(instrument.Description),
			metric.WithUnit(instrument.Unit))
		if err != nil {
			return fmt.Errorf("create histogram %s: %w", instrument.Name, err)
		}
		m.histograms[instrument.Name] = created
	case KindGauge:
		created, err := m.meter.Float64Gauge(instrument.Name,
			metric.WithDescription(instrument.Description),
			metric.WithUnit(instrument.Unit))
		if err != nil {
			return fmt.Errorf("create gauge %s: %w", instrument.Name, err)
		}
		m.gauges[instrument.Name] = created
	case KindObservableCounter:
		// Observable instruments are registered with their callback by the
		// collector that owns the state they read, not here.
	}
	return nil
}

// Meter exposes the meter so a collector can register observable instruments
// against the same instrumentation scope.
func (m *Metrics) Meter() metric.Meter { return m.meter }

// add increments a counter. An instrument absent from the catalogue is a
// programming error, and silently dropping the measurement would hide it, so
// construction has already guaranteed every catalogued name exists.
func (m *Metrics) add(ctx context.Context, name string, delta int64, attrs ...attribute.KeyValue) {
	counter, ok := m.counters[name]
	if !ok {
		return
	}
	counter.Add(ctx, delta, metric.WithAttributes(attrs...))
}

func (m *Metrics) record(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue) {
	histogram, ok := m.histograms[name]
	if !ok {
		return
	}
	histogram.Record(ctx, value, metric.WithAttributes(attrs...))
}

func (m *Metrics) set(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue) {
	gauge, ok := m.gauges[name]
	if !ok {
		return
	}
	gauge.Record(ctx, value, metric.WithAttributes(attrs...))
}
