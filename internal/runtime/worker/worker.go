// Package worker composes and runs the background processing process.
//
// As with the API runtime, the composition is importable so tests start the
// same wiring the binary starts. cmd/worker keeps only configuration, signal
// handling and the exit code.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/adapters/payments/stripe"
	"github.com/rmotti/payments-boilerplate/internal/adapters/postgres/repositories"
	"github.com/rmotti/payments-boilerplate/internal/adapters/rabbitmq"
	consumerapp "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	outboxapp "github.com/rmotti/payments-boilerplate/internal/application/outbox"
	"github.com/rmotti/payments-boilerplate/internal/platform/buildinfo"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/database"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	"github.com/rmotti/payments-boilerplate/internal/platform/metrics"
	"github.com/rmotti/payments-boilerplate/internal/platform/retry"
	"github.com/rmotti/payments-boilerplate/internal/platform/telemetry"
	httpserver "github.com/rmotti/payments-boilerplate/internal/transport/http"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// ServiceName and DefaultAddress are the identity and fallback bind address of
// this process. The address serves health only; the worker has no public API.
const (
	ServiceName    = "payments-worker"
	DefaultAddress = ":8081"
)

// Options carries the dependencies a caller may substitute. Every field is
// optional: a zero Options produces exactly the process the binary runs.
type Options struct {
	// Listener, when set, is served instead of binding cfg.HTTPAddress for the
	// health endpoint. The caller keeps ownership of it.
	Listener net.Listener

	// Interpreter replaces the Stripe payload reader. It is the port through
	// which a test decides what a stored event means, without a provider SDK
	// or a network in the path.
	Interpreter consumerapp.Interpreter

	// Logger replaces the logger built from configuration. A caller that
	// supplies one owns its lifecycle, so Run does not sync it.
	Logger *zap.Logger

	// InstanceID overrides the outbox lease owner. It exists so a test can
	// name the instance it is asserting about; left empty, one unique to this
	// process is generated.
	InstanceID string
}

// Run composes the worker and serves until ctx is cancelled.
//
// The relay, the consumer and the health server share a context derived from
// ctx, so whichever stops first takes the others down with it, and every
// connection opened here is closed before Run returns.
func Run(ctx context.Context, cfg config.Config, opts Options) error {
	logger := opts.Logger
	if logger == nil {
		built, err := logging.New(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = built.Sync() }()
		logger = built
	}

	shutdownTelemetry, err := telemetry.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			logger.Error("telemetry shutdown failed", zap.Error(err))
		}
	}()

	// Telemetry is configured first, so the instruments below are created from
	// whichever MeterProvider it installed. With OTLP disabled that is the
	// global no-op provider: every instrument is created and every observer is
	// wired exactly the same way, and nothing is recorded. No code path in the
	// relay or the consumer asks whether metrics are enabled.
	appMetrics, err := metrics.New()
	if err != nil {
		return fmt.Errorf("build metrics: %w", err)
	}

	startupCtx, cancelStartup := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancelStartup()
	var db *database.Database
	if err := retry.Do(startupCtx, 500*time.Millisecond, 5*time.Second, func() error {
		var openErr error
		db, openErr = database.Open(startupCtx, cfg, logger)
		return openErr
	}); err != nil {
		return fmt.Errorf("initialize postgres: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("postgres shutdown failed", zap.Error(err))
		}
	}()
	if err := metrics.RegisterPoolMetrics(appMetrics, db.SQL, logger); err != nil {
		return fmt.Errorf("instrument postgres pool: %w", err)
	}

	// The broker is not required to boot. Messages are already durable in
	// PostgreSQL, and the connection redials on demand, so a broker that is
	// down at startup must not stop the worker from coming up: refusing to
	// start would only mean the relay is absent when the broker returns.
	broker := rabbitmq.New(cfg.RabbitMQURL, cfg.ServiceName+"-relay")
	if err := retry.Do(startupCtx, 500*time.Millisecond, 5*time.Second, func() error {
		return broker.Connect(startupCtx)
	}); err != nil {
		logger.Warn("rabbitmq unavailable at startup; the relay will keep retrying",
			zap.Error(err))
	}
	defer func() {
		if err := broker.Close(); err != nil {
			logger.Error("rabbitmq shutdown failed", zap.Error(err))
		}
	}()

	// The consumer and its republications get connections of their own. The
	// Connection installs deadlines on the whole socket during an operation,
	// which is right for a publisher on a per-attempt budget and fatal for a
	// consumer that legitimately waits an unbounded time for a delivery. See
	// ADR 0013.
	consumerBroker := rabbitmq.New(cfg.RabbitMQURL, cfg.ServiceName+"-consumer")
	defer func() {
		if err := consumerBroker.Close(); err != nil {
			logger.Error("consumer connection shutdown failed", zap.Error(err))
		}
	}()
	republishBroker := rabbitmq.New(cfg.RabbitMQURL, cfg.ServiceName+"-republish")
	defer func() {
		if err := republishBroker.Close(); err != nil {
			logger.Error("republish connection shutdown failed", zap.Error(err))
		}
	}()

	// Queue depths are sampled over a connection of their own. Two reasons:
	// a passive declaration of a queue that does not exist closes the channel
	// it ran on, which must never be the consuming channel, and the sampler's
	// timeout installs a socket deadline that would otherwise interrupt a
	// delivery or a republication in flight.
	samplingBroker := rabbitmq.New(cfg.RabbitMQURL, cfg.ServiceName+"-metrics")
	defer func() {
		if err := samplingBroker.Close(); err != nil {
			logger.Error("metrics sampling connection shutdown failed", zap.Error(err))
		}
	}()

	// The relay is a component of the worker, not a process of its own: the
	// worker is where asynchronous work lives, and the consumer will sit
	// beside it. Both keep independent configuration and lifecycles, which is
	// what keeps extracting a cmd/relay cheap if that is ever needed.
	// See ADR 0012.
	publisher := rabbitmq.NewPublisher(broker)
	// Warm the channel so the first message does not spend its budget on
	// setup. A broker that is still down simply warms later, on demand.
	if err := publisher.Warm(startupCtx); err != nil {
		logger.Warn("outbox publisher not ready yet", zap.Error(err))
	}
	defer func() {
		if err := publisher.Close(); err != nil {
			logger.Error("publisher shutdown failed", zap.Error(err))
		}
	}()

	// The lease records which instance holds a message, so the identifier has
	// to be unique per process, not per machine: two workers on one host would
	// otherwise share an owner, and a stale one could settle a message the
	// other had already taken over. The hostname keeps it readable in logs;
	// the random suffix is what makes it correct.
	instanceID := opts.InstanceID
	if instanceID == "" {
		generated, err := InstanceIdentity(cfg.ServiceName)
		if err != nil {
			return fmt.Errorf("build relay identity: %w", err)
		}
		instanceID = generated
	}
	outboxRepository := repositories.NewOutboxRepository(db.SQL)
	relay := outboxapp.NewService(
		outboxRepository,
		publisher,
		instanceID,
		outboxapp.Config{
			BatchSize:          cfg.OutboxBatchSize,
			Interval:           cfg.OutboxInterval,
			LeaseDuration:      cfg.OutboxLeaseDuration,
			AlertAfterAttempts: cfg.OutboxAlertAfterAttempts,
			Backoff: outboxapp.Backoff{
				Base: cfg.OutboxBackoffBase,
				Max:  cfg.OutboxBackoffMax,
			},
		},
		outboxapp.WithObserver(relayObserver{logger: logger}),
		outboxapp.WithObserver(metrics.NewRelayObserver(appMetrics)),
	)

	retryTiers := make([]rabbitmq.RetryTier, 0, len(cfg.ConsumerRetryDelays))
	for _, delay := range cfg.ConsumerRetryDelays {
		retryTiers = append(retryTiers, rabbitmq.RetryTier{Delay: delay})
	}
	interpreter := opts.Interpreter
	if interpreter == nil {
		interpreter = stripe.NewSession()
	}
	eventRepository := repositories.NewEventRepository(db.SQL)
	consumerService := consumerapp.NewService(
		eventRepository,
		interpreter,
		consumerapp.Config{MaxAttempts: cfg.ConsumerMaxAttempts},
		consumerapp.WithObserver(consumerObserver{logger: logger}),
		consumerapp.WithObserver(metrics.NewConsumerObserver(appMetrics)),
	)
	// The queue label is bounded by the topology this process declares: the
	// main queue, the dead letter and one queue per configured retry tier.
	// Anything else a destination could name becomes "other".
	knownQueues := []string{rabbitmq.WebhooksQueue, rabbitmq.DeadLetterQueue}
	for _, tier := range retryTiers {
		knownQueues = append(knownQueues, tier.Queue())
	}
	messageConsumer := rabbitmq.NewConsumer(consumerBroker, republishBroker, consumerService,
		rabbitmq.ConsumerConfig{
			Prefetch:    cfg.ConsumerPrefetch,
			Concurrency: cfg.ConsumerConcurrency,
			RetryTiers:  retryTiers,
		}).
		WithObserver(brokerObserver{logger: logger}).
		WithObserver(metrics.NewBrokerObserver(appMetrics, knownQueues))

	backlogSampler := metrics.NewSampler(metrics.SamplerBacklog, appMetrics, logger,
		metrics.NewBacklogCollector(outboxRepository, eventRepository),
		metrics.SamplerConfig{Interval: cfg.MetricsSampleInterval, Timeout: cfg.MetricsSampleTimeout})
	brokerSampler := metrics.NewSampler(metrics.SamplerBroker, appMetrics, logger,
		metrics.NewQueueDepthCollector(func(ctx context.Context, queues []string) (map[string]int, error) {
			return rabbitmq.QueueDepths(ctx, samplingBroker, queues)
		}, knownQueues),
		metrics.SamplerConfig{Interval: cfg.MetricsSampleInterval, Timeout: cfg.MetricsSampleTimeout})

	healthService := health.New(cfg.ServiceName, buildinfo.Version, map[string]health.Checker{
		"postgres": db.Ping,
		"rabbitmq": broker.Ping,
	})
	// Only the health route exists on this listener. The rest of the contract
	// is not registered, so it is not reachable, rather than reachable and
	// refused. See ADR 0016.
	server := httpserver.NewHealthOnly(httpserver.Config{
		Address:         cfg.HTTPAddress,
		ShutdownTimeout: cfg.ShutdownTimeout,
		TrustedProxies:  cfg.TrustedProxies,
		Listener:        opts.Listener,
	}, logger, healthService)

	logger.Info("worker starting",
		zap.String("health_address", cfg.HTTPAddress),
		zap.String("version", buildinfo.Version),
		zap.String("commit", buildinfo.Commit),
		zap.String("instance", instanceID),
		zap.Int("outbox_batch_size", relay.Config().BatchSize),
		zap.Duration("outbox_interval", relay.Config().Interval),
		zap.Duration("outbox_lease", relay.Config().LeaseDuration),
		zap.Int("consumer_concurrency", cfg.ConsumerConcurrency),
		zap.Int("consumer_prefetch", cfg.ConsumerPrefetch),
		zap.Int("consumer_max_attempts", consumerService.Config().MaxAttempts),
		zap.Durations("consumer_retry_delays", cfg.ConsumerRetryDelays),
	)

	// Both components share a context derived from the caller's, so whichever
	// stops first takes the other down with it. Without this, a health server
	// that failed to bind would leave the relay running forever.
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		relayLogger := logger.With(zap.String("component", "outbox_relay"))
		relayLogger.Info("outbox relay started")
		defer relayLogger.Info("outbox relay stopped")
		return relay.Run(groupCtx, func(err error) {
			// A failing cycle is expected while the broker is down. The relay
			// keeps going because the messages stay safely in PostgreSQL.
			relayLogger.Error("outbox relay cycle failed", zap.Error(err))
		})
	})
	group.Go(func() error {
		consumerLogger := logger.With(zap.String("component", "consumer"))
		consumerLogger.Info("consumer started")
		defer consumerLogger.Info("consumer stopped")
		return messageConsumer.Run(groupCtx, func(err error) {
			// A dead session is expected while the broker is down. Messages
			// stay on the queue, unacknowledged, and come back on reconnect.
			consumerLogger.Error("consumer session ended", zap.Error(err))
		})
	})
	// The samplers run beside the relay and the consumer, and their Run never
	// returns an error: a backlog or depth that cannot be read must not take
	// the worker down, because the work itself is unaffected.
	group.Go(func() error { return backlogSampler.Run(groupCtx) })
	group.Go(func() error { return brokerSampler.Run(groupCtx) })
	group.Go(func() error { return server.Run(groupCtx) })
	return group.Wait()
}

// InstanceIdentity builds an owner unique to this process. Two workers on one
// machine must not share it, or a stale process could settle a message that
// another has already taken over.
func InstanceIdentity(fallback string) (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generate instance suffix: %w", err)
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = fallback
	}
	return host + "-" + hex.EncodeToString(suffix), nil
}
