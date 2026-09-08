// Package api composes and runs the public HTTP API process.
//
// The composition lives here, and not in cmd/api, because a package named
// main cannot be imported. Keeping it importable is what lets an end-to-end
// test start the very same wiring the binary starts, instead of a parallel
// approximation of it that can drift.
package api

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/adapters/catalog"
	stripeadapter "github.com/rmotti/payments-boilerplate/internal/adapters/payments/stripe"
	"github.com/rmotti/payments-boilerplate/internal/adapters/postgres/repositories"
	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
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

// ServiceName and DefaultAddress are the identity and fallback bind address of
// this process. They live beside the composition so the binary and a test load
// configuration the same way.
const (
	ServiceName    = "payments-api"
	DefaultAddress = ":8080"
)

// Options carries the dependencies a caller may substitute. Every field is
// optional: a zero Options produces exactly the process the binary runs.
//
// The substitutable dependencies are the ones that would otherwise reach
// outside the machine. They are typed as the application's own ports, so a
// test replaces the provider without a production environment variable
// existing only to point the SDK somewhere else.
type Options struct {
	// Listener, when set, is served instead of binding cfg.HTTPAddress. The
	// caller keeps ownership: Run serves it and shuts the server down, which
	// closes it, but never closes it on a path where the server never ran.
	Listener net.Listener

	// PaymentProvider replaces the Stripe checkout adapter.
	PaymentProvider paymentapp.Provider

	// WebhookVerifier replaces the Stripe signature verifier. Leaving it nil
	// keeps the real verification against STRIPE_WEBHOOK_SECRET, which is what
	// makes a locally signed event exercise the real check.
	WebhookVerifier webhookapp.Verifier

	// Logger replaces the logger built from configuration. A caller that
	// supplies one owns its lifecycle, so Run does not sync it.
	Logger *zap.Logger
}

// Run composes the API and serves until ctx is cancelled.
//
// Every resource it opens is released before it returns, so a cancelled
// context leaves no goroutine, connection or file handle behind.
func Run(ctx context.Context, cfg config.Config, opts Options) error {
	apiKeyVerifier, err := auth.NewAPIKeyVerifier(cfg.IntegrationAPIKeys)
	if err != nil {
		return fmt.Errorf("configure integration authentication: %w", err)
	}
	if err := validateStripeConfiguration(cfg, opts); err != nil {
		return err
	}

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

	provider := opts.PaymentProvider
	if provider == nil {
		provider = stripeadapter.NewCheckout(cfg.StripeSecretKey, cfg.StripeSuccessURL, cfg.StripeCancelURL)
	}
	checkoutService := paymentapp.NewService(
		orderService,
		repositories.NewPaymentRepository(db.GORM),
		provider,
	)

	verifier := opts.WebhookVerifier
	if verifier == nil {
		verifier = stripeadapter.NewWebhook(cfg.StripeWebhookSecret)
	}
	webhookRepository := repositories.NewWebhookRepository(db.SQL)
	webhookService := webhookapp.NewService(verifier, webhookRepository)
	operationsService := webhookapp.NewOperationsService(webhookRepository)

	apiHandler := httpserver.NewAPIHandler(healthService, orderService, checkoutService, webhookService, operationsService)
	docsMode := httpserver.DocsModeFor(cfg.DocsEnabled, cfg.IsDevelopment())
	if docsMode == httpserver.DocsProtected {
		// The key itself is never logged; only the fact that the routes exist.
		logger.Warn("api documentation enabled outside development; /docs and /openapi.yaml require a valid X-API-Key",
			zap.String("environment", cfg.Environment))
	}
	server := httpserver.New(httpserver.Config{
		Address:         cfg.HTTPAddress,
		ShutdownTimeout: cfg.ShutdownTimeout,
		Docs:            docsMode,
		TrustedProxies:  cfg.TrustedProxies,
		Listener:        opts.Listener,
	}, logger, apiHandler, apiKeyVerifier)

	logger.Info("api starting",
		zap.String("address", cfg.HTTPAddress),
		zap.String("version", buildinfo.Version),
		zap.String("commit", buildinfo.Commit),
	)
	return server.Run(ctx)
}

// validateStripeConfiguration follows the same boundary as Options: replacing
// one application port removes only that adapter's configuration requirement.
// In particular, a fake checkout provider must not silently disable validation
// of the real webhook verifier.
func validateStripeConfiguration(cfg config.Config, opts Options) error {
	if opts.PaymentProvider == nil {
		if err := cfg.ValidateStripeCheckout(); err != nil {
			return fmt.Errorf("configure Stripe checkout: %w", err)
		}
	}
	if opts.WebhookVerifier == nil {
		if err := cfg.ValidateStripeWebhook(); err != nil {
			return fmt.Errorf("configure Stripe webhook: %w", err)
		}
	}
	return nil
}
