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

func TestStripeValidationsAreIndependent(t *testing.T) {
	t.Parallel()

	checkout := Config{
		StripeSecretKey:  "sk_test_example",
		StripeSuccessURL: "http://localhost:3000/payment/success",
		StripeCancelURL:  "https://example.com/payment/cancel",
	}
	if err := checkout.ValidateStripeCheckout(); err != nil {
		t.Fatalf("ValidateStripeCheckout() error = %v", err)
	}
	if err := checkout.ValidateStripeWebhook(); err == nil {
		t.Fatal("ValidateStripeWebhook() error = nil without a webhook secret")
	}

	webhook := Config{StripeWebhookSecret: "whsec_example"}
	if err := webhook.ValidateStripeWebhook(); err != nil {
		t.Fatalf("ValidateStripeWebhook() error = %v", err)
	}
	if err := webhook.ValidateStripeCheckout(); err == nil {
		t.Fatal("ValidateStripeCheckout() error = nil without checkout configuration")
	}
}

func TestLoadDisablesDocsByDefaultInEveryEnvironment(t *testing.T) {
	for _, environment := range []string{"development", "test", "staging", "production"} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example")
			t.Setenv("APP_ENV", environment)
			t.Setenv("DOCS_ENABLED", "")

			cfg, err := Load("test-api", ":8000", false)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.DocsEnabled {
				t.Fatalf("DocsEnabled = true in %s without opt-in", environment)
			}
			if got, want := cfg.IsDevelopment(), environment == EnvironmentDevelopment; got != want {
				t.Fatalf("IsDevelopment() = %t in %s, want %t", got, environment, want)
			}
		})
	}
}

func TestLoadParsesDocsOptIn(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("DOCS_ENABLED", "true")

	cfg, err := Load("test-api", ":8000", false)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.DocsEnabled {
		t.Fatal("DocsEnabled = false with DOCS_ENABLED=true")
	}
}

func TestLoadParsesTrustedProxyCIDRs(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("TRUSTED_PROXY_CIDRS", " 10.1.2.3/8, fd00::1/8 ,")

	cfg, err := Load("test-api", ":8000", false)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("TrustedProxies = %v, want two masked prefixes", cfg.TrustedProxies)
	}
	if cfg.TrustedProxies[0].String() != "10.0.0.0/8" || cfg.TrustedProxies[1].String() != "fd00::/8" {
		t.Fatalf("TrustedProxies = %v, want 10.0.0.0/8 and fd00::/8", cfg.TrustedProxies)
	}
}

func TestLoadWithoutTrustedProxiesTrustsNoOne(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("TRUSTED_PROXY_CIDRS", "")

	cfg, err := Load("test-api", ":8000", false)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("TrustedProxies = %v, want none", cfg.TrustedProxies)
	}
}

func TestLoadRejectsInvalidTrustedProxyCIDRs(t *testing.T) {
	for _, value := range []string{"10.0.0.1", "10.0.0.0/33", "proxy.internal", "10.0.0.0/8;192.168.0.0/16"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example")
			t.Setenv("TRUSTED_PROXY_CIDRS", value)

			if _, err := Load("test-api", ":8000", false); err == nil {
				t.Fatalf("Load() accepted TRUSTED_PROXY_CIDRS=%q", value)
			}
		})
	}
}
