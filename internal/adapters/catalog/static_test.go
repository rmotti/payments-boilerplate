package catalog

import (
	"context"
	"errors"
	"testing"

	app "github.com/rmotti/payments-boilerplate/internal/application/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
)

func TestDemoCatalog(t *testing.T) {
	t.Parallel()

	product, err := Demo().Product(context.Background(), DemoProductID)
	if err != nil {
		t.Fatalf("Product() error = %v", err)
	}
	if product.UnitAmount != 10000 || product.Currency != domain.BRL {
		t.Fatalf("Product() = %#v, want 10000 BRL", product)
	}

	if _, err := Demo().Product(context.Background(), "missing"); !errors.Is(err, app.ErrProductNotFound) {
		t.Fatalf("Product() error = %v, want %v", err, app.ErrProductNotFound)
	}
}

func TestNewStaticRejectsBadProducts(t *testing.T) {
	t.Parallel()

	if _, err := NewStatic(domain.Product{ID: "free", UnitAmount: 0, Currency: domain.BRL}); err == nil {
		t.Fatal("NewStatic() error = nil, want an error for a free product")
	}
	valid := domain.Product{ID: "p", UnitAmount: 1, Currency: domain.BRL}
	if _, err := NewStatic(valid, valid); err == nil {
		t.Fatal("NewStatic() error = nil, want an error for a duplicated product")
	}
}
