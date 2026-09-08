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

// Rate limiting is on by default with values a legitimate integrator will not
// notice. The defaults are asserted because "safe by default" is a promise the
// project makes to whoever clones it without reading every variable.
func TestLoadEnablesRateLimitingWithSafeDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")

	cfg, err := Load("payments-api", ":8080", false)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	policy := cfg.RateLimitPolicy()
	if !policy.Enabled {
		t.Fatal("rate limiting is not enabled by default")
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"client burst", policy.ClientBurst, 1200},
		{"client interval", policy.ClientInterval, time.Minute},
		{"client capacity", policy.ClientCapacity, 10000},
		{"credential burst", policy.CredentialBurst, 600},
		{"credential interval", policy.CredentialInterval, time.Minute},
		{"credential capacity", policy.CredentialCapacity, 64},
		{"webhook burst", policy.WebhookBurst, 600},
		{"webhook interval", policy.WebhookInterval, time.Minute},
		{"health burst", policy.HealthBurst, 120},
		{"health interval", policy.HealthInterval, time.Minute},
		{"health capacity", policy.HealthCapacity, 1000},
		{"idle TTL", policy.IdleTTL, 10 * time.Minute},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
}

func TestLoadOverridesRateLimitsFromTheEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("RATE_LIMIT_ENABLED", "false")
	t.Setenv("RATE_LIMIT_CLIENT_BURST", "10")
	t.Setenv("RATE_LIMIT_CLIENT_INTERVAL", "30s")
	t.Setenv("RATE_LIMIT_CLIENT_CAPACITY", "500")
	t.Setenv("RATE_LIMIT_IDLE_TTL", "5m")

	cfg, err := Load("payments-api", ":8080", false)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	policy := cfg.RateLimitPolicy()
	if policy.Enabled {
		t.Error("RATE_LIMIT_ENABLED=false did not disable rate limiting")
	}
	if policy.ClientBurst != 10 || policy.ClientInterval != 30*time.Second {
		t.Errorf("client policy = %d per %s, want 10 per 30s", policy.ClientBurst, policy.ClientInterval)
	}
	if policy.ClientCapacity != 500 {
		t.Errorf("client capacity = %d, want 500", policy.ClientCapacity)
	}
	if policy.IdleTTL != 5*time.Minute {
		t.Errorf("idle TTL = %s, want 5m", policy.IdleTTL)
	}
}

// A bad limit must stop the process at startup rather than at the first
// request, and it must do so whether or not limiting is currently enabled: a
// deployment that turns it on later should have found the typo already.
func TestLoadRejectsInvalidRateLimits(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		enabled string
	}{
		{name: "zero client burst", env: map[string]string{"RATE_LIMIT_CLIENT_BURST": "0"}},
		{name: "negative client burst", env: map[string]string{"RATE_LIMIT_CLIENT_BURST": "-1"}},
		{name: "zero client interval", env: map[string]string{"RATE_LIMIT_CLIENT_INTERVAL": "0s"}},
		{name: "zero credential burst", env: map[string]string{"RATE_LIMIT_CREDENTIAL_BURST": "0"}},
		{name: "zero webhook interval", env: map[string]string{"RATE_LIMIT_WEBHOOK_INTERVAL": "0s"}},
		{name: "zero health burst", env: map[string]string{"RATE_LIMIT_HEALTH_BURST": "0"}},
		{name: "zero client capacity", env: map[string]string{"RATE_LIMIT_CLIENT_CAPACITY": "0"}},
		{name: "zero credential capacity", env: map[string]string{"RATE_LIMIT_CREDENTIAL_CAPACITY": "0"}},
		{name: "zero health capacity", env: map[string]string{"RATE_LIMIT_HEALTH_CAPACITY": "0"}},
		{name: "negative idle TTL", env: map[string]string{"RATE_LIMIT_IDLE_TTL": "-1m"}},
		{
			name: "idle TTL shorter than a refill window",
			env:  map[string]string{"RATE_LIMIT_IDLE_TTL": "10s", "RATE_LIMIT_CLIENT_INTERVAL": "1m"},
		},
		{
			name: "interval too short to refill the burst",
			env:  map[string]string{"RATE_LIMIT_CLIENT_BURST": "1000000000", "RATE_LIMIT_CLIENT_INTERVAL": "1ns"},
		},
		{
			name:    "validated even while disabled",
			env:     map[string]string{"RATE_LIMIT_CLIENT_BURST": "0"},
			enabled: "false",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example")
			if test.enabled != "" {
				t.Setenv("RATE_LIMIT_ENABLED", test.enabled)
			}
			for name, value := range test.env {
				t.Setenv(name, value)
			}
			if _, err := Load("payments-api", ":8080", false); err == nil {
				t.Fatal("Load() accepted an invalid rate limit configuration")
			}
		})
	}
}
