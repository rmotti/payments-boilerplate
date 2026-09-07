// Package payments contains the checkout use cases and their required ports.
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	orders "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
)

const providerTimeout = 10 * time.Second

// Checkout errors surfaced by the use case.
var (
	ErrIdempotencyKeyConflict = errors.New("idempotency key already used")
	ErrCheckoutInProgress     = errors.New("another checkout is already active for this order")
	ErrOrderNotPayable        = errors.New("order is not available for checkout")
	ErrProviderUnavailable    = errors.New("payment provider unavailable")
)

// OrderReader obtains the immutable price and current commercial state.
type OrderReader interface {
	Get(ctx context.Context, id string) (orders.Order, error)
}

// Repository atomically reserves a local payment attempt and later attaches
// the external provider reference to it.
type Repository interface {
	PrepareCheckout(
		ctx context.Context,
		payment domain.Payment,
		attempt domain.Attempt,
	) (domain.Payment, domain.Attempt, error)
	SaveSession(ctx context.Context, attempt domain.Attempt) error
}

// Provider is the narrow port implemented by the Stripe adapter.
type Provider interface {
	CreateCheckout(ctx context.Context, request ProviderRequest) (domain.Session, error)
}

// ProviderRequest contains only trusted, server-side payment data.
type ProviderRequest struct {
	OrderID        string
	PaymentID      string
	AttemptID      string
	ProductID      string
	UnitAmount     int64
	Quantity       int
	Currency       orders.Currency
	IdempotencyKey string
}

// Checkout is returned to the HTTP layer so the consumer can be redirected.
type Checkout struct {
	URL       string
	ExpiresAt time.Time
}

// CreateInput identifies the order and the client operation being retried.
type CreateInput struct {
	OrderID        string
	IdempotencyKey string
}

// Service coordinates local persistence and the external provider.
type Service struct {
	orders       OrderReader
	repository   Repository
	provider     Provider
	now          func() time.Time
	newPaymentID func() (string, error)
	newAttemptID func() (string, error)
}

// Option customizes deterministic dependencies in tests.
type Option func(*Service)

// WithClock replaces the wall clock.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithIDGenerators replaces both local identifier generators.
func WithIDGenerators(paymentID, attemptID func() (string, error)) Option {
	return func(s *Service) {
		s.newPaymentID = paymentID
		s.newAttemptID = attemptID
	}
}

// NewService wires the checkout use case to its ports.
func NewService(orders OrderReader, repository Repository, provider Provider, opts ...Option) *Service {
	service := &Service{
		orders: orders, repository: repository, provider: provider, now: time.Now,
		newPaymentID: domain.NewPaymentID, newAttemptID: domain.NewAttemptID,
	}
	for _, option := range opts {
		option(service)
	}
	return service
}

// CreateCheckout creates or recovers one idempotent hosted checkout session.
func (s *Service) CreateCheckout(ctx context.Context, input CreateInput) (Checkout, error) {
	if err := orders.ValidateID(input.OrderID); err != nil {
		return Checkout{}, orderapp.ErrOrderNotFound
	}
	if err := orders.ValidateIdempotencyKey(input.IdempotencyKey); err != nil {
		return Checkout{}, err
	}

	order, err := s.orders.Get(ctx, input.OrderID)
	if err != nil {
		return Checkout{}, fmt.Errorf("load checkout order: %w", err)
	}
	if order.Status != orders.StatusPending {
		return Checkout{}, ErrOrderNotPayable
	}

	paymentID, err := s.newPaymentID()
	if err != nil {
		return Checkout{}, err
	}
	attemptID, err := s.newAttemptID()
	if err != nil {
		return Checkout{}, err
	}
	payment, attempt, err := domain.New(paymentID, attemptID, order, input.IdempotencyKey, s.now())
	if err != nil {
		return Checkout{}, err
	}

	payment, attempt, err = s.repository.PrepareCheckout(ctx, payment, attempt)
	if err != nil {
		return Checkout{}, fmt.Errorf("prepare checkout: %w", err)
	}
	if attempt.HasSession() {
		return checkoutFromAttempt(attempt), nil
	}

	providerCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	session, err := s.provider.CreateCheckout(providerCtx, ProviderRequest{
		OrderID: order.ID, PaymentID: payment.ID, AttemptID: attempt.ID,
		ProductID: order.ProductID, UnitAmount: order.Amount / int64(order.Quantity),
		Quantity: order.Quantity, Currency: order.Currency, IdempotencyKey: attempt.IdempotencyKey,
	})
	if err != nil {
		return Checkout{}, fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
	}
	attempt, err = attempt.AttachSession(session, s.now())
	if err != nil {
		return Checkout{}, fmt.Errorf("%w: validate provider session: %w", ErrProviderUnavailable, err)
	}
	if err := s.repository.SaveSession(ctx, attempt); err != nil {
		return Checkout{}, fmt.Errorf("persist provider session: %w", err)
	}
	return checkoutFromAttempt(attempt), nil
}

func checkoutFromAttempt(attempt domain.Attempt) Checkout {
	return Checkout{URL: attempt.CheckoutURL, ExpiresAt: attempt.ExpiresAt}
}
