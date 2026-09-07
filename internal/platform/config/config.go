// Package config loads and validates process configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config contains all process configuration loaded from environment variables.
type Config struct {
	ServiceName string

	Environment string `env:"APP_ENV" envDefault:"development"`
	HTTPAddress string `env:"HTTP_ADDRESS"`
	Port        string `env:"PORT"`

	DatabaseURL                   string        `env:"DATABASE_URL,required"`
	DatabaseMaxOpenConnections    int           `env:"DATABASE_MAX_OPEN_CONNECTIONS" envDefault:"10"`
	DatabaseMaxIdleConnections    int           `env:"DATABASE_MAX_IDLE_CONNECTIONS" envDefault:"5"`
	DatabaseConnectionMaxLifetime time.Duration `env:"DATABASE_CONNECTION_MAX_LIFETIME" envDefault:"30m"`

	RabbitMQURL string `env:"RABBITMQ_URL"`

	IntegrationAPIKeys []string `env:"INTEGRATION_API_KEYS"`

	StripeSecretKey     string `env:"STRIPE_SECRET_KEY"`
	StripeWebhookSecret string `env:"STRIPE_WEBHOOK_SECRET"`
	StripeSuccessURL    string `env:"STRIPE_SUCCESS_URL"`
	StripeCancelURL     string `env:"STRIPE_CANCEL_URL"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	StartupTimeout  time.Duration `env:"STARTUP_TIMEOUT" envDefault:"30s"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`

	OTelEnabled          bool          `env:"OTEL_ENABLED" envDefault:"false"`
	OTelServiceName      string        `env:"OTEL_SERVICE_NAME"`
	OTelExporterEndpoint string        `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"http://localhost:4318"`
	OTelExportInterval   time.Duration `env:"OTEL_EXPORT_INTERVAL" envDefault:"10s"`

	MigrationsDir string `env:"MIGRATIONS_DIR" envDefault:"db/migrations"`
}

// Load reads an optional local .env file and then parses process environment.
// Existing environment variables always take precedence over the file.
func Load(serviceName, defaultHTTPAddress string, requireRabbitMQ bool) (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}

	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parse environment: %w", err)
	}

	cfg.ServiceName = serviceName
	if cfg.OTelServiceName == "" {
		cfg.OTelServiceName = serviceName
	}
	if cfg.Port != "" {
		if strings.Contains(cfg.Port, ":") {
			return Config{}, errors.New("PORT must contain only a port number")
		}
		cfg.HTTPAddress = ":" + cfg.Port
	}
	if cfg.HTTPAddress == "" {
		cfg.HTTPAddress = defaultHTTPAddress
	}

	if requireRabbitMQ && cfg.RabbitMQURL == "" {
		return Config{}, errors.New("RABBITMQ_URL is required for this process")
	}
	if cfg.DatabaseMaxOpenConnections < 1 {
		return Config{}, errors.New("DATABASE_MAX_OPEN_CONNECTIONS must be positive")
	}
	if cfg.DatabaseMaxIdleConnections < 0 || cfg.DatabaseMaxIdleConnections > cfg.DatabaseMaxOpenConnections {
		return Config{}, errors.New("DATABASE_MAX_IDLE_CONNECTIONS must be between zero and the maximum open connections")
	}
	if cfg.StartupTimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		return Config{}, errors.New("startup and shutdown timeouts must be positive")
	}
	if cfg.OTelEnabled && cfg.OTelExporterEndpoint == "" {
		return Config{}, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT is required when telemetry is enabled")
	}

	return cfg, nil
}

// ValidateStripe checks the configuration required by the API checkout flow.
// It is separate from Load because the worker does not call Stripe.
func (c Config) ValidateStripe() error {
	if c.StripeSecretKey == "" {
		return errors.New("STRIPE_SECRET_KEY is required for the API")
	}
	if c.StripeWebhookSecret == "" {
		return errors.New("STRIPE_WEBHOOK_SECRET is required for the API")
	}
	if err := validateReturnURL("STRIPE_SUCCESS_URL", c.StripeSuccessURL); err != nil {
		return err
	}
	return validateReturnURL("STRIPE_CANCEL_URL", c.StripeCancelURL)
}

func validateReturnURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute HTTP or HTTPS URL", name)
	}
	return nil
}
