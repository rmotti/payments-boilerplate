// Package sandbox provides local-only tooling for reproducible Stripe test
// flows. It deliberately refuses remote endpoints and live credentials.
package sandbox

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

const defaultAPIURL = "http://localhost:8080"

// Config is the narrow environment surface used by the sandbox command.
type Config struct {
	Environment   string
	APIURL        string
	APIKey        string
	DatabaseURL   string
	StripeKey     string
	WebhookSecret string
}

// Requirements selects the credentials and dependencies a subcommand needs.
type Requirements struct {
	API           bool
	Database      bool
	Stripe        bool
	WebhookSecret bool
}

// LoadConfig loads .env without applying the production process configuration.
// Each sandbox subcommand validates only the dependencies it actually uses.
func LoadConfig() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}

	apiURL := strings.TrimSpace(os.Getenv("SANDBOX_API_URL"))
	if apiURL == "" {
		apiURL = defaultAPIURL
	}

	return Config{
		Environment:   strings.TrimSpace(os.Getenv("APP_ENV")),
		APIURL:        strings.TrimRight(apiURL, "/"),
		APIKey:        firstAPIKey(os.Getenv("INTEGRATION_API_KEYS")),
		DatabaseURL:   strings.TrimSpace(os.Getenv("DATABASE_URL")),
		StripeKey:     strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")),
		WebhookSecret: strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
	}, nil
}

// Validate enforces that sandbox traffic and data stay on a local test setup.
func (c Config) Validate(require Requirements) error {
	if c.Environment != "development" && c.Environment != "test" {
		return errors.New("sandbox requires APP_ENV=development or APP_ENV=test")
	}
	if require.API {
		if c.APIKey == "" {
			return errors.New("sandbox requires at least one INTEGRATION_API_KEYS entry")
		}
		if err := validateLoopbackHTTP(c.APIURL); err != nil {
			return err
		}
	}
	if require.Database {
		if err := validateLoopbackPostgres(c.DatabaseURL); err != nil {
			return err
		}
	}
	if require.Stripe && !strings.HasPrefix(c.StripeKey, "sk_test_") {
		return errors.New("sandbox requires a Stripe test key beginning with sk_test_")
	}
	if require.WebhookSecret && !strings.HasPrefix(c.WebhookSecret, "whsec_") {
		return errors.New("sandbox requires STRIPE_WEBHOOK_SECRET beginning with whsec_")
	}
	return nil
}

func firstAPIKey(value string) string {
	for _, candidate := range strings.Split(value, ",") {
		if key := strings.TrimSpace(candidate); key != "" {
			return key
		}
	}
	return ""
}

func validateLoopbackHTTP(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" {
		return errors.New("SANDBOX_API_URL must be an HTTP origin such as http://localhost:8080")
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return errors.New("SANDBOX_API_URL must point to localhost or a loopback address")
	}
	return nil
}

func validateLoopbackPostgres(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" {
		return errors.New("DATABASE_URL must be a PostgreSQL URL for the local sandbox")
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return errors.New("sandbox DATABASE_URL must point to localhost or a loopback address")
	}
	if strings.TrimPrefix(parsed.Path, "/") == "" {
		return errors.New("sandbox DATABASE_URL must name a database")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
