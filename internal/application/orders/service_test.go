package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
)

type fakeCatalog map[string]domain.Product

func (c fakeCatalog) Product(_ context.Context, id string) (domain.Product, error) {
	product, ok := c[id]
	if !ok {
		return domain.Product{}, ErrProductNotFound
	}
	return product, nil
}

type fakeRepository struct {
	created []domain.Order
	err     error
}

func (r *fakeRepository) Create(_ context.Context, order domain.Order) error {
	if r.err != nil {
		return r.err
	}
	r.created = append(r.created, order)
	return nil
}

var demo = domain.Product{ID: "product_demo", Name: "Demo", UnitAmount: 10000, Currency: domain.BRL}

func newService(repo *fakeRepository, opts ...Option) *Service {
	fixed := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	base := []Option{
		WithClock(func() time.Time { return fixed }),
		WithIDGenerator(func() (string, error) { return "ord_fixed", nil }),
	}
	return NewService(fakeCatalog{demo.ID: demo}, repo, append(base, opts...)...)
}

func TestCreatePricesFromCatalogAndPersists(t *testing.T) {
	t.Parallel()

	repo := &fakeRepository{}
	order, err := newService(repo).Create(context.Background(), CreateInput{
		ProductID:      "product_demo",
		Quantity:       2,
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if order.ID != "ord_fixed" || order.Amount != 20000 || order.Currency != domain.BRL || order.Status != domain.StatusPending {
		t.Fatalf("Create() = %#v, want ord_fixed pending 20000 BRL", order)
	}
	if len(repo.created) != 1 || repo.created[0] != order {
		t.Fatalf("repository received %#v, want the returned order", repo.created)
	}
}

func TestCreateRejectsInvalidInputBeforeTouchingPorts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input CreateInput
		want  error
	}{
		{name: "zero quantity", input: CreateInput{ProductID: "product_demo", Quantity: 0, IdempotencyKey: "k"}, want: domain.ErrInvalidQuantity},
		{name: "empty product", input: CreateInput{ProductID: "", Quantity: 1, IdempotencyKey: "k"}, want: domain.ErrInvalidProductID},
		{name: "empty key", input: CreateInput{ProductID: "product_demo", Quantity: 1, IdempotencyKey: ""}, want: domain.ErrInvalidIdempotencyKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeRepository{}
			_, err := newService(repo).Create(context.Background(), tt.input)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Create() error = %v, want %v", err, tt.want)
			}
			if len(repo.created) != 0 {
				t.Fatalf("repository received %d orders, want none", len(repo.created))
			}
		})
	}
}

func TestCreateUnknownProduct(t *testing.T) {
	t.Parallel()

	repo := &fakeRepository{}
	_, err := newService(repo).Create(context.Background(), CreateInput{ProductID: "missing", Quantity: 1, IdempotencyKey: "k"})
	if !errors.Is(err, ErrProductNotFound) {
		t.Fatalf("Create() error = %v, want %v", err, ErrProductNotFound)
	}
	if len(repo.created) != 0 {
		t.Fatalf("repository received %d orders, want none", len(repo.created))
	}
}

func TestCreatePropagatesIdempotencyConflict(t *testing.T) {
	t.Parallel()

	repo := &fakeRepository{err: ErrIdempotencyKeyConflict}
	_, err := newService(repo).Create(context.Background(), CreateInput{ProductID: "product_demo", Quantity: 1, IdempotencyKey: "k"})
	if !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("Create() error = %v, want %v", err, ErrIdempotencyKeyConflict)
	}
}

func TestCreateFailsWhenIDGenerationFails(t *testing.T) {
	t.Parallel()

	boom := errors.New("entropy exhausted")
	repo := &fakeRepository{}
	_, err := newService(repo, WithIDGenerator(func() (string, error) { return "", boom })).
		Create(context.Background(), CreateInput{ProductID: "product_demo", Quantity: 1, IdempotencyKey: "k"})
	if !errors.Is(err, boom) {
		t.Fatalf("Create() error = %v, want %v", err, boom)
	}
	if len(repo.created) != 0 {
		t.Fatalf("repository received %d orders, want none", len(repo.created))
	}
}
