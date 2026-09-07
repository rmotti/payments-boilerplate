// Package orders contains the order use cases and the ports they require.
package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
)

// Errors returned by ports and surfaced by the use cases.
var (
	// ErrProductNotFound is returned by a Catalog when the product is unknown.
	ErrProductNotFound = errors.New("product not found")

	// ErrIdempotencyKeyConflict is returned by a Repository when another order
	// already owns the idempotency key.
	ErrIdempotencyKeyConflict = errors.New("idempotency key already used")
)

// Catalog resolves the products the server knows how to price.
type Catalog interface {
	Product(ctx context.Context, id string) (domain.Product, error)
}

// Repository persists orders.
type Repository interface {
	Create(ctx context.Context, order domain.Order) error
}

// CreateInput is what a client is allowed to say about a new order. It carries
// no amount or currency on purpose.
type CreateInput struct {
	ProductID      string
	Quantity       int
	IdempotencyKey string
}

// Validate checks the input shape before any dependency is touched.
func (in CreateInput) Validate() error {
	if err := domain.ValidateProductID(in.ProductID); err != nil {
		return err
	}
	if err := domain.ValidateQuantity(in.Quantity); err != nil {
		return err
	}
	return domain.ValidateIdempotencyKey(in.IdempotencyKey)
}

// Service implements the order use cases.
type Service struct {
	catalog    Catalog
	repository Repository
	now        func() time.Time
	newID      func() (string, error)
}

// Option customises a Service.
type Option func(*Service)

// WithClock replaces the wall clock, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithIDGenerator replaces the identifier generator, for tests.
func WithIDGenerator(newID func() (string, error)) Option {
	return func(s *Service) { s.newID = newID }
}

// NewService wires the order use cases to their ports.
func NewService(catalog Catalog, repository Repository, opts ...Option) *Service {
	s := &Service{
		catalog:    catalog,
		repository: repository,
		now:        time.Now,
		newID:      domain.NewID,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create prices a new pending order from the catalog and persists it. The
// amount is computed here, from the product the server knows, never from the
// client.
func (s *Service) Create(ctx context.Context, in CreateInput) (domain.Order, error) {
	if err := in.Validate(); err != nil {
		return domain.Order{}, err
	}

	product, err := s.catalog.Product(ctx, in.ProductID)
	if err != nil {
		return domain.Order{}, fmt.Errorf("resolve product: %w", err)
	}

	id, err := s.newID()
	if err != nil {
		return domain.Order{}, err
	}

	order, err := domain.New(id, product, in.Quantity, in.IdempotencyKey, s.now())
	if err != nil {
		return domain.Order{}, err
	}

	if err := s.repository.Create(ctx, order); err != nil {
		return domain.Order{}, fmt.Errorf("persist order: %w", err)
	}
	return order, nil
}
