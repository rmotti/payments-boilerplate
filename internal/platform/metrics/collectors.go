package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
)

// Sampler names. They are the values the sampler label may carry.
const (
	SamplerBacklog = "backlog"
	SamplerBroker  = "broker"
)

// OutboxBacklogReader reports unpublished outbox work. It is the narrow slice
// of the outbox repository this package needs, declared here so the collector
// depends on a method set rather than on the repository type.
//
// Pending counts messages in 'pending' and in 'publishing': a message leased
// by a relay whose outcome is not yet recorded is still unpublished work, and
// excluding it would make a stalled relay look like an empty outbox.
type OutboxBacklogReader interface {
	Backlog(ctx context.Context) (pending int64, oldest time.Duration, err error)
}

// InboxBacklogReader reports inbox work waiting or stuck.
//
// Pending counts entries in 'pending' and in 'processing', for the same
// reason: an entry locked by a consumer that died is still waiting. Failed
// counts entries the consumer gave up on, which is the operator's queue.
type InboxBacklogReader interface {
	Backlog(ctx context.Context) (pending, failed int64, oldest time.Duration, err error)
}

// NewBacklogCollector samples the outbox and inbox backlog from PostgreSQL.
//
// Both reads happen in the sampler's goroutine, under its timeout, never in a
// collection callback: a database that stops answering must delay a gauge, not
// block the exporter.
func NewBacklogCollector(outbox OutboxBacklogReader, inbox InboxBacklogReader) Collector {
	return func(ctx context.Context) (Sample, error) {
		pending, oldest, err := outbox.Backlog(ctx)
		if err != nil {
			return Sample{}, fmt.Errorf("sample outbox backlog: %w", err)
		}
		inboxPending, inboxFailed, inboxOldest, err := inbox.Backlog(ctx)
		if err != nil {
			return Sample{}, fmt.Errorf("sample inbox backlog: %w", err)
		}
		return Sample{Values: []Measurement{
			{Instrument: OutboxPending, Value: float64(pending)},
			{Instrument: OutboxOldestAge, Value: oldest.Seconds()},
			{Instrument: InboxPending, Value: float64(inboxPending)},
			{Instrument: InboxFailed, Value: float64(inboxFailed)},
			{Instrument: InboxOldestAge, Value: inboxOldest.Seconds()},
		}}, nil
	}
}

// QueueDepthReader reports how many messages are ready in each named queue.
type QueueDepthReader func(ctx context.Context, queues []string) (map[string]int, error)

// NewQueueDepthCollector samples the depth of the declared queues.
//
// The queues are the ones this process declared, so the queue label cannot
// take a value the topology does not define. The read uses a connection and a
// channel the caller dedicated to sampling, never the consuming or publishing
// connection: a passive declaration of a missing queue closes the channel it
// runs on, and doing that to a production channel would interrupt delivery.
func NewQueueDepthCollector(read QueueDepthReader, queues []string) Collector {
	known := append([]string(nil), queues...)
	return func(ctx context.Context) (Sample, error) {
		depths, err := read(ctx, known)
		if err != nil {
			return Sample{}, fmt.Errorf("sample queue depths: %w", err)
		}
		values := make([]Measurement, 0, len(known))
		for _, queue := range known {
			depth, ok := depths[queue]
			if !ok {
				continue
			}
			values = append(values, Measurement{Instrument: QueueDepth, Value: float64(depth), Queue: queue})
		}
		return Sample{Values: values}, nil
	}
}

// RegisterPoolMetrics reports PostgreSQL pool usage by state.
//
// These are observable instruments rather than a sampler: database/sql keeps
// the statistics in memory and Stats is a lock-and-copy, so reading them in a
// collection callback performs no I/O and cannot block on the database.
func RegisterPoolMetrics(metrics *Metrics, db *sql.DB, logger *zap.Logger) error {
	if db == nil {
		return nil
	}
	connectionsInstrument, _ := lookup(PoolConnections)
	connections, err := metrics.Meter().Int64ObservableGauge(PoolConnections,
		metricDescription(connectionsInstrument), metricUnit(connectionsInstrument))
	if err != nil {
		return fmt.Errorf("register pool gauge: %w", err)
	}
	maxOpenInstrument, _ := lookup(PoolMaxOpen)
	maxOpen, err := metrics.Meter().Int64ObservableGauge(PoolMaxOpen,
		metricDescription(maxOpenInstrument), metricUnit(maxOpenInstrument))
	if err != nil {
		return fmt.Errorf("register pool ceiling gauge: %w", err)
	}
	waitsInstrument, _ := lookup(PoolWaits)
	waits, err := metrics.Meter().Int64ObservableCounter(PoolWaits,
		metricDescription(waitsInstrument), metricUnit(waitsInstrument))
	if err != nil {
		return fmt.Errorf("register pool wait counter: %w", err)
	}

	inUse := observeAttributes(attribute.String(LabelState, "in_use"))
	idle := observeAttributes(attribute.String(LabelState, "idle"))
	if _, err := metrics.Meter().RegisterCallback(
		func(_ context.Context, observer metricObserver) error {
			stats := db.Stats()
			observer.ObserveInt64(connections, int64(stats.InUse), inUse)
			observer.ObserveInt64(connections, int64(stats.Idle), idle)
			observer.ObserveInt64(maxOpen, int64(stats.MaxOpenConnections))
			observer.ObserveInt64(waits, stats.WaitCount)
			return nil
		}, connections, maxOpen, waits); err != nil {
		return fmt.Errorf("register pool callback: %w", err)
	}
	if logger != nil {
		logger.Debug("postgres pool metrics registered", zap.String("component", "metrics"))
	}
	return nil
}
