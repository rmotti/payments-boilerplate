package models

import (
	"testing"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
)

func TestOrderRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 6, 15, 0, 0, 0, time.FixedZone("BRT", -3*60*60))
	order, err := domain.New("ord_1", domain.Product{ID: "p", UnitAmount: 250, Currency: domain.BRL}, 4, "key", now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got := OrderFromDomain(order).ToDomain()
	if got != order {
		t.Fatalf("round trip = %#v, want %#v", got, order)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt location = %s, want UTC", got.CreatedAt.Location())
	}
}
