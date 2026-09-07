package orders

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

var demo = Product{ID: "product_demo", Name: "Demo", UnitAmount: 10000, Currency: BRL}

func TestNewPricesOrderOnTheServer(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.FixedZone("BRT", -3*60*60))
	order, err := New("ord_test", demo, 3, "key-1", now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if order.Amount != 30000 || order.Currency != BRL {
		t.Fatalf("New() amount = %d %s, want 30000 BRL", order.Amount, order.Currency)
	}
	if order.Status != StatusPending {
		t.Fatalf("New() status = %q, want %q", order.Status, StatusPending)
	}
	if order.ProductID != demo.ID || order.Quantity != 3 || order.IdempotencyKey != "key-1" {
		t.Fatalf("New() = %#v, want product, quantity and key preserved", order)
	}
	if order.CreatedAt.Location() != time.UTC || !order.CreatedAt.Equal(now) || !order.UpdatedAt.Equal(now) {
		t.Fatalf("New() timestamps = %s/%s, want %s in UTC", order.CreatedAt, order.UpdatedAt, now)
	}
}

func TestNewRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []struct {
		name     string
		id       string
		product  Product
		quantity int
		key      string
		want     error
	}{
		{name: "zero quantity", id: "ord_1", product: demo, quantity: 0, key: "k", want: ErrInvalidQuantity},
		{name: "negative quantity", id: "ord_1", product: demo, quantity: -1, key: "k", want: ErrInvalidQuantity},
		{name: "quantity above max", id: "ord_1", product: demo, quantity: MaxQuantity + 1, key: "k", want: ErrInvalidQuantity},
		{name: "empty key", id: "ord_1", product: demo, quantity: 1, key: "", want: ErrInvalidIdempotencyKey},
		{name: "padded key", id: "ord_1", product: demo, quantity: 1, key: " k ", want: ErrInvalidIdempotencyKey},
		{name: "long key", id: "ord_1", product: demo, quantity: 1, key: strings.Repeat("k", MaxIdempotencyKeyLength+1), want: ErrInvalidIdempotencyKey},
		{name: "free product", id: "ord_1", product: Product{ID: "p", UnitAmount: 0, Currency: BRL}, quantity: 1, key: "k", want: ErrInvalidProduct},
		{name: "negative price", id: "ord_1", product: Product{ID: "p", UnitAmount: -1, Currency: BRL}, quantity: 1, key: "k", want: ErrInvalidProduct},
		{name: "lowercase currency", id: "ord_1", product: Product{ID: "p", UnitAmount: 1, Currency: "brl"}, quantity: 1, key: "k", want: ErrInvalidProduct},
		{name: "empty product id", id: "ord_1", product: Product{ID: "", UnitAmount: 1, Currency: BRL}, quantity: 1, key: "k", want: ErrInvalidProductID},
		{name: "overflow", id: "ord_1", product: Product{ID: "p", UnitAmount: math.MaxInt64, Currency: BRL}, quantity: 2, key: "k", want: ErrAmountOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tt.id, tt.product, tt.quantity, tt.key, now); !errors.Is(err, tt.want) {
				t.Fatalf("New() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNewRequiresID(t *testing.T) {
	t.Parallel()

	if _, err := New("", demo, 1, "k", time.Now()); err == nil {
		t.Fatal("New() error = nil, want an error for an empty id")
	}
}

func TestNewIDIsOpaqueAndUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 100)
	for range 100 {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID() error = %v", err)
		}
		if !strings.HasPrefix(id, IDPrefix) || len(id) != len(IDPrefix)+32 {
			t.Fatalf("NewID() = %q, want %q prefix and 32 hex characters", id, IDPrefix)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestValidateID(t *testing.T) {
	t.Parallel()

	valid := "ord_0123456789abcdef0123456789abcdef"
	if err := ValidateID(valid); err != nil {
		t.Fatalf("ValidateID(%q) error = %v", valid, err)
	}
	for _, id := range []string{"", "ord_short", "pay_0123456789abcdef0123456789abcdef", "ord_0123456789abcdef0123456789abcdeg"} {
		if err := ValidateID(id); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("ValidateID(%q) error = %v, want %v", id, err, ErrInvalidID)
		}
	}
}

func TestCurrencyValid(t *testing.T) {
	t.Parallel()

	for code, want := range map[Currency]bool{"BRL": true, "USD": true, "brl": false, "BR": false, "BRLL": false, "": false} {
		if got := code.Valid(); got != want {
			t.Fatalf("Currency(%q).Valid() = %v, want %v", code, got, want)
		}
	}
}

func TestMarkPaid(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		status Status
		want   Status
		moved  bool
	}{
		{name: "pending order is paid", status: StatusPending, want: StatusPaid, moved: true},
		{name: "paid order does not move again", status: StatusPaid, want: StatusPaid},
		{name: "cancelled order is not revived", status: StatusCancelled, want: StatusCancelled},
		{name: "expired order is not revived", status: StatusExpired, want: StatusExpired},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, moved := Order{ID: "ord_1", Status: test.status}.MarkPaid(now)
			if got.Status != test.want || moved != test.moved {
				t.Errorf("MarkPaid() = %q, %t, want %q, %t", got.Status, moved, test.want, test.moved)
			}
			if moved && !got.UpdatedAt.Equal(now) {
				t.Errorf("MarkPaid() UpdatedAt = %v, want %v", got.UpdatedAt, now)
			}
		})
	}
}
