package metrics

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
)

// With OTEL_ENABLED false the telemetry package installs no MeterProvider, so
// the global one stays the no-op. Everything must still construct and record
// without changing behaviour: this is the guarantee that disabling export does
// not disable the application.
func TestNewUsesTheGlobalProviderAndWorksWhenItIsNoop(t *testing.T) {
	// Not parallel: it reads the global MeterProvider, which other tests in
	// this binary do not touch but a future one might.
	otel.SetMeterProvider(noop.NewMeterProvider())

	built, err := New()
	if err != nil {
		t.Fatalf("New() with a no-op provider error = %v", err)
	}

	exercise(built)
	exerciseWithHostileInput(built)

	if err := RegisterPoolMetrics(built, nil, nil); err != nil {
		t.Fatalf("RegisterPoolMetrics() error = %v", err)
	}

	// A sampler must still run its collector: the work is not conditional on
	// export, only the recording is.
	collected := make(chan struct{}, 1)
	sampler := NewSampler(SamplerBacklog, built, nil,
		func(context.Context) (Sample, error) {
			select {
			case collected <- struct{}{}:
			default:
			}
			return Sample{Values: []Measurement{{Instrument: OutboxPending, Value: 3}}}, nil
		},
		SamplerConfig{Interval: time.Hour, Timeout: time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sampler.Run(ctx) }()
	select {
	case <-collected:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the sampler did not collect with export disabled")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := sampler.Failures(); got != 0 {
		t.Errorf("failures = %d, want 0", got)
	}
}
