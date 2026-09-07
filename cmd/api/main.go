// Command api starts the public HTTP API.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/adapters/catalog"
	stripeadapter "github.com/rmotti/payments-boilerplate/internal/adapters/payments/stripe"
	"github.com/rmotti/payments-boilerplate/internal/adapters/postgres/repositories"
	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	"github.com/rmotti/payments-boilerplate/internal/platform/auth"
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
	cfg, err := config.Load("payments-api", ":8080", false)
	if err != nil {
		return err
	}
	apiKeyVerifier, err := auth.NewAPIKeyVerifier(cfg.IntegrationAPIKeys)
	if err != nil {
		return fmt.Errorf("configure integration authentication: %w", err)
	}
	if err := cfg.ValidateStripe(); err != nil {
		return fmt.Errorf("configure Stripe: %w", err)
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

	healthService := health.New(cfg.ServiceName, buildinfo.Version, map[string]health.Checker{
		"postgres": db.Ping,
	})
	orderService := orderapp.NewService(catalog.Demo(), repositories.NewOrderRepository(db.GORM))
	checkoutService := paymentapp.NewService(
		orderService,
		repositories.NewPaymentRepository(db.GORM),
		stripeadapter.NewCheckout(cfg.StripeSecretKey, cfg.StripeSuccessURL, cfg.StripeCancelURL),
	)
	apiHandler := httpserver.NewAPIHandler(healthService, orderService, checkoutService)
	server := httpserver.New(httpserver.Config{
		Address:         cfg.HTTPAddress,
		ShutdownTimeout: cfg.ShutdownTimeout,
		DocsEnabled:     true,
	}, logger, apiHandler, apiKeyVerifier)

	logger.Info("api starting",
		zap.String("address", cfg.HTTPAddress),
		zap.String("version", buildinfo.Version),
		zap.String("commit", buildinfo.Commit),
	)
	return server.Run(ctx)
}
