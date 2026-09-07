// Command worker starts background message processing.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"syscall"
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
	"github.com/rmotti/payments-boilerplate/internal/platform/retry"
	"github.com/rmotti/payments-boilerplate/internal/platform/telemetry"
	httpserver "github.com/rmotti/payments-boilerplate/internal/transport/http"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load("payments-worker", ":8081", true)
	if err != nil {
		return err
	}
	logger, err := logging.New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
	instanceID, err := instanceIdentity(cfg.ServiceName)
	if err != nil {
		return fmt.Errorf("build relay identity: %w", err)
	}
	relay := outboxapp.NewService(
		repositories.NewOutboxRepository(db.SQL),
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
	)

	retryTiers := make([]rabbitmq.RetryTier, 0, len(cfg.ConsumerRetryDelays))
	for _, delay := range cfg.ConsumerRetryDelays {
		retryTiers = append(retryTiers, rabbitmq.RetryTier{Delay: delay})
	}
	consumerService := consumerapp.NewService(
		repositories.NewEventRepository(db.SQL),
		stripe.NewSession(),
		consumerapp.Config{MaxAttempts: cfg.ConsumerMaxAttempts},
		consumerapp.WithObserver(consumerObserver{logger: logger}),
	)
	messageConsumer := rabbitmq.NewConsumer(consumerBroker, republishBroker, consumerService,
		rabbitmq.ConsumerConfig{
			Prefetch:    cfg.ConsumerPrefetch,
			Concurrency: cfg.ConsumerConcurrency,
			RetryTiers:  retryTiers,
		}).WithObserver(brokerObserver{logger: logger})

	healthService := health.New(cfg.ServiceName, buildinfo.Version, map[string]health.Checker{
		"postgres": db.Ping,
		"rabbitmq": broker.Ping,
	})
	apiHandler := httpserver.NewAPIHandler(healthService, nil, nil, nil)
	server := httpserver.New(httpserver.Config{
		Address:         cfg.HTTPAddress,
		ShutdownTimeout: cfg.ShutdownTimeout,
		DocsEnabled:     false,
	}, logger, apiHandler, nil)

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

	// Both components share a context derived from the signal context, so
	// whichever stops first takes the other down with it. Without this, a
	// health server that failed to bind would leave the relay running forever.
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
	group.Go(func() error { return server.Run(groupCtx) })
	return group.Wait()
}

// relayObserver turns relay findings into logs. A stuck message keeps being
// retried; surfacing it is what lets an operator notice before a customer does.
type relayObserver struct{ logger *zap.Logger }

func (o relayObserver) Stuck(messageID string, attempts int, cause error) {
	o.logger.Warn("outbox message is not going through",
		zap.String("component", "outbox_relay"),
		zap.String("message_id", messageID),
		zap.Int("attempts", attempts),
		zap.Error(cause),
	)
}

func (o relayObserver) Abandoned(messageID string, attempts int, cause error) {
	o.logger.Error("outbox message abandoned after a permanent failure",
		zap.String("component", "outbox_relay"),
		zap.String("message_id", messageID),
		zap.Int("attempts", attempts),
		zap.Error(cause),
	)
}

func (o relayObserver) LeaseLost(messageID string, cause error) {
	o.logger.Warn("outbox lease expired before the outcome could be recorded",
		zap.String("component", "outbox_relay"),
		zap.String("message_id", messageID),
		zap.Error(cause),
	)
}

// consumerObserver turns consumer decisions into logs. Metrics for these
// belong to Phase 4; the relay set the same precedent.
type consumerObserver struct{ logger *zap.Logger }

func (o consumerObserver) Applied(eventID string, effect consumerapp.Effect) {
	o.logger.Info("webhook event applied",
		zap.String("component", "consumer"),
		zap.String("webhook_event_id", eventID),
		zap.String("attempt_status", string(effect.AttemptStatus)),
		zap.String("payment_status", string(effect.PaymentStatus)),
		zap.Bool("order_paid", effect.OrderPaid),
	)
}

func (o consumerObserver) NoOp(eventID, note string) {
	o.logger.Info("webhook event produced no change",
		zap.String("component", "consumer"),
		zap.String("webhook_event_id", eventID),
		zap.String("reason", note),
	)
}

func (o consumerObserver) Retrying(eventID string, attempts int, cause error) {
	o.logger.Warn("webhook event will be retried",
		zap.String("component", "consumer"),
		zap.String("webhook_event_id", eventID),
		zap.Int("attempts", attempts),
		zap.Error(cause),
	)
}

func (o consumerObserver) DeadLettered(eventID string, attempts int, cause error) {
	o.logger.Error("webhook event sent to the dead-letter queue",
		zap.String("component", "consumer"),
		zap.String("webhook_event_id", eventID),
		zap.Int("attempts", attempts),
		zap.Error(cause),
	)
}

// brokerObserver reports what the consumer did with the message itself, as
// opposed to the event it carried.
type brokerObserver struct{ logger *zap.Logger }

func (o brokerObserver) Rejected(messageID string, cause error) {
	o.logger.Error("message could not be read and was rejected",
		zap.String("component", "consumer"),
		zap.String("message_id", messageID),
		zap.Error(cause),
	)
}

func (o brokerObserver) Republished(messageID, destination string, attempts int) {
	o.logger.Info("message republished",
		zap.String("component", "consumer"),
		zap.String("message_id", messageID),
		zap.String("destination", destination),
		zap.Int("attempts", attempts),
	)
}

func (o brokerObserver) Failed(messageID string, cause error) {
	// Not acknowledged, so the broker still owns it. This is the safe outcome
	// of an unknown one, but it must be visible.
	o.logger.Error("message left unacknowledged for redelivery",
		zap.String("component", "consumer"),
		zap.String("message_id", messageID),
		zap.Error(cause),
	)
}

// instanceIdentity builds an owner unique to this process. Two workers on one
// machine must not share it, or a stale process could settle a message that
// another has already taken over.
func instanceIdentity(fallback string) (string, error) {
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
