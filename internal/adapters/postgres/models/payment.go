package models

import (
	"time"

	orders "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
)

// Payment maps the payments table.
type Payment struct {
	ID        string    `gorm:"primaryKey"`
	OrderID   string    `gorm:"not null"`
	Provider  string    `gorm:"not null"`
	Status    string    `gorm:"not null"`
	Amount    int64     `gorm:"not null"`
	Currency  string    `gorm:"not null"`
	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName pins the database table name.
func (Payment) TableName() string { return "payments" }

// PaymentFromDomain converts a domain payment for persistence.
func PaymentFromDomain(payment domain.Payment) Payment {
	return Payment{
		ID: payment.ID, OrderID: payment.OrderID, Provider: string(payment.Provider),
		Status: string(payment.Status), Amount: payment.Amount, Currency: string(payment.Currency),
		CreatedAt: payment.CreatedAt.UTC(), UpdatedAt: payment.UpdatedAt.UTC(),
	}
}

// ToDomain converts a persisted payment into the domain entity.
func (p Payment) ToDomain() domain.Payment {
	return domain.Payment{
		ID: p.ID, OrderID: p.OrderID, Provider: domain.Provider(p.Provider),
		Status: domain.Status(p.Status), Amount: p.Amount, Currency: orders.Currency(p.Currency),
		CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
	}
}

// PaymentAttempt maps the payment_attempts table.
type PaymentAttempt struct {
	ID                      string `gorm:"primaryKey"`
	PaymentID               string `gorm:"not null"`
	Provider                string `gorm:"not null"`
	Status                  string `gorm:"not null"`
	IdempotencyKey          string `gorm:"not null"`
	ProviderSessionID       *string
	ProviderPaymentIntentID *string
	CheckoutURL             *string `gorm:"column:checkout_url"`
	ExpiresAt               *time.Time
	FailureCode             *string
	FailureMessage          *string
	CreatedAt               time.Time `gorm:"not null"`
	UpdatedAt               time.Time `gorm:"not null"`
}

// TableName pins the database table name.
func (PaymentAttempt) TableName() string { return "payment_attempts" }

// PaymentAttemptFromDomain converts a domain attempt for persistence.
func PaymentAttemptFromDomain(attempt domain.Attempt) PaymentAttempt {
	row := PaymentAttempt{
		ID: attempt.ID, PaymentID: attempt.PaymentID, Provider: string(attempt.Provider),
		Status: string(attempt.Status), IdempotencyKey: attempt.IdempotencyKey,
		CreatedAt: attempt.CreatedAt.UTC(), UpdatedAt: attempt.UpdatedAt.UTC(),
	}
	if attempt.ProviderSessionID != "" {
		row.ProviderSessionID = &attempt.ProviderSessionID
	}
	if attempt.ProviderPaymentIntentID != "" {
		row.ProviderPaymentIntentID = &attempt.ProviderPaymentIntentID
	}
	if attempt.CheckoutURL != "" {
		row.CheckoutURL = &attempt.CheckoutURL
	}
	if !attempt.ExpiresAt.IsZero() {
		expiresAt := attempt.ExpiresAt.UTC()
		row.ExpiresAt = &expiresAt
	}
	return row
}

// ToDomain converts a persisted attempt into the domain entity.
func (a PaymentAttempt) ToDomain() domain.Attempt {
	attempt := domain.Attempt{
		ID: a.ID, PaymentID: a.PaymentID, Provider: domain.Provider(a.Provider),
		Status: domain.AttemptStatus(a.Status), IdempotencyKey: a.IdempotencyKey,
		CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(),
	}
	if a.ProviderSessionID != nil {
		attempt.ProviderSessionID = *a.ProviderSessionID
	}
	if a.ProviderPaymentIntentID != nil {
		attempt.ProviderPaymentIntentID = *a.ProviderPaymentIntentID
	}
	if a.CheckoutURL != nil {
		attempt.CheckoutURL = *a.CheckoutURL
	}
	if a.ExpiresAt != nil {
		attempt.ExpiresAt = a.ExpiresAt.UTC()
	}
	return attempt
}
