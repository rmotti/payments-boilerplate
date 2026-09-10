package sandbox

import "testing"

func TestConfigAcceptsOnlyLocalTestSurfaces(t *testing.T) {
	t.Parallel()

	valid := Config{
		Environment: "development", APIURL: "http://127.0.0.1:8080",
		APIKey: "sandbox_integration_key", DatabaseURL: "postgres://user:pass@localhost:5432/payments",
		StripeKey: "sk_test_example", WebhookSecret: "whsec_example",
	}
	requirements := Requirements{API: true, Database: true, Stripe: true, WebhookSecret: true}
	if err := valid.Validate(requirements); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"production environment", func(cfg *Config) { cfg.Environment = "production" }},
		{"remote API", func(cfg *Config) { cfg.APIURL = "https://api.example.com" }},
		{"remote database", func(cfg *Config) { cfg.DatabaseURL = "postgres://user:pass@db.example.com/payments" }},
		{"live Stripe key", func(cfg *Config) { cfg.StripeKey = "sk_live_example" }},
		{"missing webhook secret", func(cfg *Config) { cfg.WebhookSecret = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := valid
			test.change(&candidate)
			if err := candidate.Validate(requirements); err == nil {
				t.Fatal("Validate() error = nil, want sandbox safety rejection")
			}
		})
	}
}

func TestFirstAPIKeySkipsEmptyEntries(t *testing.T) {
	t.Parallel()
	if got := firstAPIKey(" , first-key , second-key"); got != "first-key" {
		t.Fatalf("firstAPIKey() = %q, want first-key", got)
	}
}
