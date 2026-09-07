// Package payments holds the financial entities and invariants shared by the
// checkout and webhook use cases.
package payments

import (
	"errors"
	"net/url"
	"strings"
	"time"

	orders "github.com/rmotti/payments-boilerplate/internal/domain/orders"
)

// Provider identifies the external payment provider used by a payment.
type Provider string

// Stripe is the first supported payment provider.
const Stripe Provider = "stripe"

// Status is the consolidated financial state of a payment.
type Status string

// Payment statuses.
const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
	// Refund states are already part of the persisted payment vocabulary. They
	// are terminal for checkout events: a late session event must never move a
	// refunded payment back to processing, failed or succeeded.
	StatusPartiallyRefunded Status = "partially_refunded"
	StatusRefunded          Status = "refunded"
)

// AttemptStatus is the state of one interaction with the provider.
type AttemptStatus string

// Payment attempt statuses.
const (
	AttemptStatusCreated   AttemptStatus = "created"
	AttemptStatusPending   AttemptStatus = "pending"
	AttemptStatusSucceeded AttemptStatus = "succeeded"
	AttemptStatusFailed    AttemptStatus = "failed"
	AttemptStatusExpired   AttemptStatus = "expired"
	AttemptStatusCancelled AttemptStatus = "cancelled"
)

// Domain validation errors.
var (
	ErrInvalidPayment = errors.New("invalid payment")
	ErrInvalidAttempt = errors.New("invalid payment attempt")
	ErrInvalidSession = errors.New("invalid checkout session")
)

// Payment is the local financial intent tied to an immutable priced order.
type Payment struct {
	ID        string
	OrderID   string
	Provider  Provider
	Status    Status
	Amount    int64
	Currency  orders.Currency
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Attempt records one idempotent request to open a provider checkout.
type Attempt struct {
	ID                      string
	PaymentID               string
	Provider                Provider
	Status                  AttemptStatus
	IdempotencyKey          string
	ProviderSessionID       string
	ProviderPaymentIntentID string
	CheckoutURL             string
	ExpiresAt               time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Session is the provider reference returned after opening a hosted checkout.
type Session struct {
	ID              string
	PaymentIntentID string
	URL             string
	ExpiresAt       time.Time
}

// New creates a pending local payment and its first unsubmitted attempt.
func New(paymentID, attemptID string, order orders.Order, idempotencyKey string, now time.Time) (Payment, Attempt, error) {
	if strings.TrimSpace(paymentID) == "" || strings.TrimSpace(attemptID) == "" ||
		order.ID == "" || order.Amount <= 0 || !order.Currency.Valid() || orders.ValidateQuantity(order.Quantity) != nil {
		return Payment{}, Attempt{}, ErrInvalidPayment
	}
	if err := orders.ValidateIdempotencyKey(idempotencyKey); err != nil {
		return Payment{}, Attempt{}, ErrInvalidAttempt
	}

	now = now.UTC()
	payment := Payment{
		ID: paymentID, OrderID: order.ID, Provider: Stripe, Status: StatusPending,
		Amount: order.Amount, Currency: order.Currency, CreatedAt: now, UpdatedAt: now,
	}
	attempt := Attempt{
		ID: attemptID, PaymentID: paymentID, Provider: Stripe, Status: AttemptStatusCreated,
		IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now,
	}
	return payment, attempt, nil
}

// AttachSession records a validated provider response on an attempt.
func (a Attempt) AttachSession(session Session, now time.Time) (Attempt, error) {
	parsed, err := url.Parse(session.URL)
	if strings.TrimSpace(session.ID) == "" || err != nil || parsed.Scheme != "https" || parsed.Host == "" || !session.ExpiresAt.After(now) {
		return Attempt{}, ErrInvalidSession
	}
	a.Status = AttemptStatusPending
	a.ProviderSessionID = session.ID
	a.ProviderPaymentIntentID = session.PaymentIntentID
	a.CheckoutURL = session.URL
	a.ExpiresAt = session.ExpiresAt.UTC()
	a.UpdatedAt = now.UTC()
	return a, nil
}

// HasSession reports whether the provider response was already persisted.
func (a Attempt) HasSession() bool {
	return a.Status == AttemptStatusPending && a.ProviderSessionID != "" && a.CheckoutURL != "" && !a.ExpiresAt.IsZero()
}
