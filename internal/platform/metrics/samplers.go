package metrics

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
)

// Default sampler timings. They are deliberately coarse: these are backlog
// and depth signals, not per-request latency, and sampling them faster only
// adds load to the database and the broker.
const (
	DefaultSampleInterval = 15 * time.Second
	DefaultSampleTimeout  = 5 * time.Second
)

// Sample is one collection round of a background sampler.
//
// Collect runs in a goroutine of the Sampler's own, never inside an
// OpenTelemetry collection callback. That is the rule this type exists to
// enforce: a callback runs on the exporter's thread and must not open a
// socket, wait on a database or take a lock a broker outage can hold. Here
// the export path only reads the last value this goroutine published.
type Sample struct {
	// Values are the measurements of this round, keyed by whatever the
	// sampler chose. Publish turns them into gauge values.
	Values []Measurement
}

// Measurement is one gauge value with its normalized attributes.
type Measurement struct {
	Instrument string
	Value      float64
	// Queue, when set, is recorded as the queue label. It is the only
	// dimension a sampler may vary, and the sampler validates it against the
	// declared topology before it gets here.
	Queue string
}

// Collector produces one sample. It is called with a context that already
// carries the sampler's timeout, so an unreachable dependency ends the round
// instead of blocking it forever.
type Collector func(ctx context.Context) (Sample, error)

// Sampler runs a Collector on an interval and publishes the last value it
// managed to collect.
//
// A failed round changes nothing: the previously published gauges stay as they
// are, the failure counter goes up, and the age gauge grows until a round
// succeeds again. This is what makes an outage of the database or the broker
// visible without either blocking collection or stopping the process.
type Sampler struct {
	name      string
	collect   Collector
	metrics   *Metrics
	logger    *zap.Logger
	interval  time.Duration
	timeout   time.Duration
	now       func() time.Time
	failures  atomic.Int64
	lastMu    sync.Mutex
	lastOK    time.Time
	started   time.Time
	observing sync.Once
}

// SamplerConfig tunes one sampler.
type SamplerConfig struct {
	Interval time.Duration
	Timeout  time.Duration
	// Now replaces the clock, for tests.
	Now func() time.Time
}

func (c SamplerConfig) withDefaults() SamplerConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultSampleInterval
	}
	if c.Timeout <= 0 || c.Timeout > c.Interval {
		c.Timeout = DefaultSampleTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// NewSampler creates a sampler. name identifies it in the sampler labels and
// must be one of the values the allowlist knows.
func NewSampler(
	name string,
	metrics *Metrics,
	logger *zap.Logger,
	collect Collector,
	config SamplerConfig,
) *Sampler {
	config = config.withDefaults()
	sampler := &Sampler{
		name: name, collect: collect, metrics: metrics, logger: logger,
		interval: config.Interval, timeout: config.Timeout, now: config.Now,
	}
	sampler.started = config.Now()
	return sampler
}

// Name reports the sampler's label value.
func (s *Sampler) Name() string { return s.name }

// Run samples until ctx is cancelled. It always returns nil: a sampler that
// cannot collect must never take the process down with it, because the work it
// observes is unaffected by the observation failing.
func (s *Sampler) Run(ctx context.Context) error {
	s.registerObservers()

	// Collect once immediately so the first export carries real values rather
	// than an empty series for a whole interval.
	s.sample(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.sample(ctx)
		}
	}
}

// sample runs one round under the sampler's own timeout.
func (s *Sampler) sample(ctx context.Context) {
	sampleCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	sample, err := s.collect(sampleCtx)
	if err != nil {
		// A cancelled parent is a shutdown, not a collection failure.
		if ctx.Err() != nil {
			return
		}
		s.failures.Add(1)
		if s.logger != nil {
			s.logger.Warn("metrics sample failed; publishing the last known values",
				zap.String("component", "metrics_sampler"),
				zap.String("sampler", s.name),
				zap.Error(err))
		}
		return
	}

	for _, measurement := range sample.Values {
		if measurement.Queue == "" {
			s.metrics.set(sampleCtx, measurement.Instrument, measurement.Value)
			continue
		}
		// The queue label is not normalized against a static allowlist: its
		// values are the topology's own queue names. The collector already
		// restricted them to the queues this process declared, which is the
		// bound that matters.
		s.metrics.set(sampleCtx, measurement.Instrument, measurement.Value,
			attribute.String(LabelQueue, measurement.Queue))
	}

	s.lastMu.Lock()
	s.lastOK = s.now()
	s.lastMu.Unlock()
}

// registerObservers wires the sampler's own health instruments. They are
// observable, so they report the current age and failure count at collection
// time without the sampler having to push them on a schedule of its own.
func (s *Sampler) registerObservers() {
	s.observing.Do(func() {
		instrument, _ := lookup(SamplerAge)
		age, err := s.metrics.Meter().Float64ObservableGauge(SamplerAge,
			metricDescription(instrument), metricUnit(instrument))
		if err != nil {
			s.logError("register sampler age gauge", err)
			return
		}
		failuresInstrument, _ := lookup(SamplerFailures)
		failures, err := s.metrics.Meter().Int64ObservableCounter(SamplerFailures,
			metricDescription(failuresInstrument), metricUnit(failuresInstrument))
		if err != nil {
			s.logError("register sampler failure counter", err)
			return
		}
		attrs := observeAttributes(Attr(LabelSampler, s.name))
		if _, err := s.metrics.Meter().RegisterCallback(
			func(_ context.Context, observer metricObserver) error {
				observer.ObserveFloat64(age, s.ageSeconds(), attrs)
				observer.ObserveInt64(failures, s.failures.Load(), attrs)
				return nil
			}, age, failures); err != nil {
			s.logError("register sampler callback", err)
		}
	})
}

// ageSeconds reports how long it has been since a successful round. Before the
// first success it counts from construction, so a sampler that never worked is
// as visible as one that stopped working.
func (s *Sampler) ageSeconds() float64 {
	s.lastMu.Lock()
	last := s.lastOK
	s.lastMu.Unlock()
	if last.IsZero() {
		last = s.started
	}
	return s.now().Sub(last).Seconds()
}

// Failures reports how many rounds failed. It exists for tests and for the
// health of the sampler itself.
func (s *Sampler) Failures() int64 { return s.failures.Load() }

func (s *Sampler) logError(what string, err error) {
	if s.logger != nil {
		s.logger.Error("metrics sampler could not be instrumented",
			zap.String("component", "metrics_sampler"),
			zap.String("sampler", s.name),
			zap.String("operation", what),
			zap.Error(err))
	}
}
