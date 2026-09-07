package payments

import (
	"errors"
	"testing"
	"time"

	orders "github.com/rmotti/payments-boilerplate/internal/domain/orders"
)

func TestNewCreatesPendingStripePaymentAndAttempt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.FixedZone("BRT", -3*60*60))
	order := orders.Order{ID: "ord_1", Amount: 20000, Currency: orders.BRL, Quantity: 2}
	payment, attempt, err := New("pay_1", "pat_1", order, "checkout-key", now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if payment.OrderID != order.ID || payment.Amount != order.Amount || payment.Currency != orders.BRL ||
		payment.Provider != Stripe || payment.Status != StatusPending {
		t.Fatalf("payment = %#v, want pending Stripe payment for order", payment)
	}
	if attempt.PaymentID != payment.ID || attempt.Status != AttemptStatusCreated || attempt.IdempotencyKey != "checkout-key" {
		t.Fatalf("attempt = %#v, want created attempt", attempt)
	}
	if payment.CreatedAt.Location() != time.UTC || attempt.CreatedAt.Location() != time.UTC {
		t.Fatal("timestamps must be normalized to UTC")
	}
}

func TestAttachSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)
	attempt := Attempt{ID: "pat_1", Status: AttemptStatusCreated}
	got, err := attempt.AttachSession(Session{
		ID: "cs_test_1", PaymentIntentID: "pi_1", URL: "https://checkout.stripe.com/c/pay/test", ExpiresAt: expires,
	}, now)
	if err != nil {
		t.Fatalf("AttachSession() error = %v", err)
	}
	if !got.HasSession() || got.ProviderSessionID != "cs_test_1" || got.ProviderPaymentIntentID != "pi_1" {
		t.Fatalf("AttachSession() = %#v, want persisted session", got)
	}
}

func TestAttachSessionRejectsUnsafeOrIncompleteReference(t *testing.T) {
	t.Parallel()

	for _, session := range []Session{
		{URL: "https://checkout.stripe.com/test", ExpiresAt: time.Now()},
		{ID: "cs_1", URL: "http://checkout.stripe.com/test", ExpiresAt: time.Now()},
		{ID: "cs_1", URL: "https://checkout.stripe.com/test"},
	} {
		if _, err := (Attempt{}).AttachSession(session, time.Now()); !errors.Is(err, ErrInvalidSession) {
			t.Fatalf("AttachSession(%#v) error = %v, want %v", session, err, ErrInvalidSession)
		}
	}
}
