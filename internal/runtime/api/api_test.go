package api

import (
	"context"
	"testing"

	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	paymentdomain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	webhookdomain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
)

type providerStub struct{}

func (providerStub) CreateCheckout(context.Context, paymentapp.ProviderRequest) (paymentdomain.Session, error) {
	return paymentdomain.Session{}, nil
}

type verifierStub struct{}

func (verifierStub) Verify([]byte, string) (webhookapp.ProviderEvent, error) {
	return webhookapp.ProviderEvent{}, nil
}

func (verifierStub) Provider() webhookdomain.Provider { return webhookdomain.Stripe }

func TestValidateStripeConfigurationFollowsInjectedPorts(t *testing.T) {
	t.Parallel()

	valid := config.Config{
		StripeSecretKey:     "sk_test_example",
		StripeWebhookSecret: "whsec_example",
		StripeSuccessURL:    "https://example.test/success",
		StripeCancelURL:     "https://example.test/cancel",
	}

	tests := []struct {
		name    string
		cfg     config.Config
		opts    Options
		wantErr bool
	}{
		{name: "both real", cfg: valid},
		{
			name: "fake checkout still validates real webhook",
			cfg:  config.Config{StripeWebhookSecret: valid.StripeWebhookSecret},
			opts: Options{PaymentProvider: providerStub{}},
		},
		{
			name:    "fake checkout rejects missing real webhook secret",
			opts:    Options{PaymentProvider: providerStub{}},
			wantErr: true,
		},
		{
			name: "fake webhook still validates real checkout",
			cfg: config.Config{
				StripeSecretKey:  valid.StripeSecretKey,
				StripeSuccessURL: valid.StripeSuccessURL,
				StripeCancelURL:  valid.StripeCancelURL,
			},
			opts: Options{WebhookVerifier: verifierStub{}},
		},
		{
			name:    "fake webhook rejects missing real checkout configuration",
			opts:    Options{WebhookVerifier: verifierStub{}},
			wantErr: true,
		},
		{
			name: "both fake need no Stripe configuration",
			opts: Options{PaymentProvider: providerStub{}, WebhookVerifier: verifierStub{}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateStripeConfiguration(test.cfg, test.opts)
			if test.wantErr && err == nil {
				t.Fatal("validateStripeConfiguration() error = nil, want validation failure")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateStripeConfiguration() error = %v", err)
			}
		})
	}
}
