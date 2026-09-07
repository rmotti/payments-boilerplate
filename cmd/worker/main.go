// Command worker starts background message processing.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/adapters/rabbitmq"
	"github.com/rmotti/payments-boilerplate/internal/platform/buildinfo"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/database"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	"github.com/rmotti/payments-boilerplate/internal/platform/retry"
	"github.com/rmotti/payments-boilerplate/internal/platform/telemetry"
	httpserver "github.com/rmotti/payments-boilerplate/internal/transport/http"
	"go.uber.org/zap"
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

	var broker *rabbitmq.Connection
	if err := retry.Do(startupCtx, 500*time.Millisecond, 5*time.Second, func() error {
		var openErr error
		broker, openErr = rabbitmq.Open(cfg.RabbitMQURL, cfg.ServiceName)
		return openErr
	}); err != nil {
		return fmt.Errorf("initialize rabbitmq: %w", err)
	}
	defer func() {
		if err := broker.Close(); err != nil {
			logger.Error("rabbitmq shutdown failed", zap.Error(err))
		}
	}()

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
	)
	return server.Run(ctx)
}
