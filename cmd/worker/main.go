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

	"github.com/rmotti/payments-boilerplate/internal/adapters/postgres/repositories"
	"github.com/rmotti/payments-boilerplate/internal/adapters/rabbitmq"
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
	broker := rabbitmq.New(cfg.RabbitMQURL, cfg.ServiceName)
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
