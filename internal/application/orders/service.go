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

	// ErrOrderNotFound is returned when the requested order does not exist.
	ErrOrderNotFound = errors.New("order not found")
)

// Catalog resolves the products the server knows how to price.
type Catalog interface {
	Product(ctx context.Context, id string) (domain.Product, error)
}

// Repository persists orders.
type Repository interface {
	CreateOrGet(ctx context.Context, order domain.Order) (persisted domain.Order, created bool, err error)
	Get(ctx context.Context, id string) (domain.Order, error)
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

// Observer receives idempotency outcomes the use case would otherwise keep
// to itself.
type Observer interface {
	// Replayed reports a request whose idempotency key matched an equivalent
	// earlier order, which was returned instead of a new one.
	Replayed()
	// Conflicted reports an idempotency key reused with a different request.
	Conflicted()
}

// Service implements the order use cases.
type Service struct {
	catalog    Catalog
	repository Repository
	observer   Observer
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

// WithObserver reports idempotent replays and conflicts.
func WithObserver(observer Observer) Option {
	return func(s *Service) { s.observer = observer }
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

	persisted, created, err := s.repository.CreateOrGet(ctx, order)
	if err != nil {
		return domain.Order{}, fmt.Errorf("persist order: %w", err)
	}
	if !created && (persisted.ProductID != in.ProductID || persisted.Quantity != in.Quantity) {
		if s.observer != nil {
			s.observer.Conflicted()
		}
		return domain.Order{}, ErrIdempotencyKeyConflict
	}
	if !created && s.observer != nil {
		s.observer.Replayed()
	}
	return persisted, nil
}

// Get returns the current local state of an order.
func (s *Service) Get(ctx context.Context, id string) (domain.Order, error) {
	if err := domain.ValidateID(id); err != nil {
		return domain.Order{}, ErrOrderNotFound
	}
	order, err := s.repository.Get(ctx, id)
	if err != nil {
		return domain.Order{}, fmt.Errorf("get order: %w", err)
	}
	return order, nil
}
