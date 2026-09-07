// Package catalog provides the products the server is able to price. The
// first version ships a fixed in-memory catalog; product management is out of
// the 0.1 scope.
package catalog

import (
	"context"
	"fmt"

	app "github.com/rmotti/payments-boilerplate/internal/application/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
)

// DemoProductID is the identifier of the product available out of the box.
const DemoProductID = "product_demo"

// Static is an immutable in-memory catalog.
type Static struct {
	products map[string]domain.Product
}

// NewStatic builds a catalog from a fixed set of products. Every product is
// validated up front so a misconfigured price fails at startup, not at the
// first order.
func NewStatic(products ...domain.Product) (*Static, error) {
	index := make(map[string]domain.Product, len(products))
	for _, product := range products {
		if err := product.Validate(); err != nil {
			return nil, fmt.Errorf("catalog product %q: %w", product.ID, err)
		}
		if _, dup := index[product.ID]; dup {
			return nil, fmt.Errorf("catalog product %q: duplicated", product.ID)
		}
		index[product.ID] = product
	}
	return &Static{products: index}, nil
}

// Demo returns the catalog used by the documentation and the sandbox: one
// product priced at R$ 100,00.
func Demo() *Static {
	static, err := NewStatic(domain.Product{
		ID:         DemoProductID,
		Name:       "Produto de demonstracao",
		UnitAmount: 10000,
		Currency:   domain.BRL,
	})
	if err != nil {
		panic(err) // The demo product is a constant; failing here is a programming error.
	}
	return static
}

// Product resolves a product by identifier.
func (s *Static) Product(_ context.Context, id string) (domain.Product, error) {
	product, ok := s.products[id]
	if !ok {
		return domain.Product{}, app.ErrProductNotFound
	}
	return product, nil
}
