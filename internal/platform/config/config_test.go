package config

import (
	"testing"
	"time"
)

func TestLoadUsesRailwayPort(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("PORT", "9090")
	t.Setenv("HTTP_ADDRESS", ":8080")

	cfg, err := Load("test-service", ":8000", false)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddress != ":9090" {
		t.Fatalf("HTTPAddress = %q, want %q", cfg.HTTPAddress, ":9090")
	}
	if cfg.OTelServiceName != "test-service" {
		t.Fatalf("OTelServiceName = %q, want %q", cfg.OTelServiceName, "test-service")
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 15s", cfg.ShutdownTimeout)
	}
}

func TestLoadRequiresRabbitMQWhenRequested(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("RABBITMQ_URL", "")

	if _, err := Load("test-worker", ":8001", true); err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
}

func TestLoadParsesIntegrationAPIKeys(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("INTEGRATION_API_KEYS", "current-key,next-key")

	cfg, err := Load("test-api", ":8000", false)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.IntegrationAPIKeys) != 2 || cfg.IntegrationAPIKeys[0] != "current-key" || cfg.IntegrationAPIKeys[1] != "next-key" {
		t.Fatalf("IntegrationAPIKeys = %#v, want current-key and next-key", cfg.IntegrationAPIKeys)
	}
}

func TestLoadRequiresLeaseToIncludeSettlementReserve(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("OUTBOX_BATCH_SIZE", "20")
	t.Setenv("OUTBOX_LEASE_DURATION", "100s")

	if _, err := Load("test-worker", ":8001", false); err == nil {
		t.Fatal("Load() error = nil, want a lease without settlement reserve to be rejected")
	}

	t.Setenv("OUTBOX_LEASE_DURATION", "105s")
	if _, err := Load("test-worker", ":8001", false); err != nil {
		t.Fatalf("Load() with publish budget and settlement reserve error = %v", err)
	}
}

func TestValidateStripe(t *testing.T) {
	t.Parallel()

	valid := Config{
		StripeSecretKey: "sk_test_example", StripeWebhookSecret: "whsec_example",
		StripeSuccessURL: "http://localhost:3000/payment/success",
		StripeCancelURL:  "https://example.com/payment/cancel",
	}
	if err := valid.ValidateStripe(); err != nil {
		t.Fatalf("ValidateStripe() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing secret", mutate: func(c *Config) { c.StripeSecretKey = "" }},
		{name: "missing webhook secret", mutate: func(c *Config) { c.StripeWebhookSecret = "" }},
		{name: "relative success", mutate: func(c *Config) { c.StripeSuccessURL = "/success" }},
		{name: "unsupported cancel scheme", mutate: func(c *Config) { c.StripeCancelURL = "javascript:alert(1)" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			tt.mutate(&cfg)
			if err := cfg.ValidateStripe(); err == nil {
				t.Fatal("ValidateStripe() error = nil, want validation failure")
			}
		})
	}
}
