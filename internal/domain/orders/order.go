// Package orders holds the commercial intent of a purchase and the invariants
// that keep its price under the server's control.
package orders

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Status is the commercial state of an order. It is deliberately not the
// financial state of the charge: a declined card leaves the order pending.
type Status string

// Order statuses.
const (
	StatusPending   Status = "pending"
	StatusPaid      Status = "paid"
	StatusCancelled Status = "cancelled"
	StatusExpired   Status = "expired"
)

// Limits enforced on every order.
const (
	// MaxQuantity bounds how many units a single order may carry. It keeps
	// the amount far from overflow and blocks accidental bulk orders.
	MaxQuantity = 1000

	// MaxIdempotencyKeyLength bounds the client-chosen key.
	MaxIdempotencyKeyLength = 255

	// MaxProductIDLength bounds the product identifier accepted from clients.
	MaxProductIDLength = 128
)

// Currency is an ISO 4217 alphabetic code.
type Currency string

// BRL is the only currency supported by the first version.
const BRL Currency = "BRL"

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Valid reports whether the currency has the ISO 4217 alphabetic shape.
func (c Currency) Valid() bool {
	return currencyPattern.MatchString(string(c))
}

// Domain errors. Callers compare them with errors.Is.
var (
	ErrInvalidProductID      = errors.New("product id must be between 1 and 128 characters")
	ErrInvalidQuantity       = fmt.Errorf("quantity must be between 1 and %d", MaxQuantity)
	ErrInvalidIdempotencyKey = errors.New("idempotency key must be between 1 and 255 characters")
	ErrInvalidProduct        = errors.New("product has an invalid price or currency")
	ErrAmountOverflow        = errors.New("order amount overflows the supported range")
)

// Product is the catalog entry an order is priced from. The unit amount is an
// integer in the minor unit of the currency.
type Product struct {
	ID         string
	Name       string
	UnitAmount int64
	Currency   Currency
}

// Validate checks that the product can price an order.
func (p Product) Validate() error {
	if !validProductID(p.ID) {
		return ErrInvalidProductID
	}
	if p.UnitAmount <= 0 || !p.Currency.Valid() {
		return ErrInvalidProduct
	}
	return nil
}

// Order is the commercial intent. Amount and currency are derived from the
// product and quantity on the server and are never accepted from a client.
type Order struct {
	ID             string
	Status         Status
	Amount         int64
	Currency       Currency
	ProductID      string
	Quantity       int
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// New prices a pending order from a catalog product.
func New(id string, product Product, quantity int, idempotencyKey string, now time.Time) (Order, error) {
	if strings.TrimSpace(id) == "" {
		return Order{}, errors.New("order id is required")
	}
	if err := product.Validate(); err != nil {
		return Order{}, err
	}
	if err := ValidateQuantity(quantity); err != nil {
		return Order{}, err
	}
	if err := ValidateIdempotencyKey(idempotencyKey); err != nil {
		return Order{}, err
	}

	amount, err := multiply(product.UnitAmount, int64(quantity))
	if err != nil {
		return Order{}, err
	}

	now = now.UTC()
	return Order{
		ID:             id,
		Status:         StatusPending,
		Amount:         amount,
		Currency:       product.Currency,
		ProductID:      product.ID,
		Quantity:       quantity,
		IdempotencyKey: idempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// ValidateProductID checks the shape of a product identifier supplied by a
// client, before the catalog is consulted.
func ValidateProductID(id string) error {
	if !validProductID(id) {
		return ErrInvalidProductID
	}
	return nil
}

// ValidateQuantity checks that a quantity is within the accepted range.
func ValidateQuantity(quantity int) error {
	if quantity < 1 || quantity > MaxQuantity {
		return ErrInvalidQuantity
	}
	return nil
}

// ValidateIdempotencyKey checks the shape of a client-chosen idempotency key.
func ValidateIdempotencyKey(key string) error {
	if key == "" || len(key) > MaxIdempotencyKeyLength || strings.TrimSpace(key) != key {
		return ErrInvalidIdempotencyKey
	}
	return nil
}

func validProductID(id string) bool {
	return id != "" && len(id) <= MaxProductIDLength && strings.TrimSpace(id) == id
}

func multiply(unitAmount, quantity int64) (int64, error) {
	if unitAmount > math.MaxInt64/quantity {
		return 0, ErrAmountOverflow
	}
	return unitAmount * quantity, nil
}

// Settled reports whether the order reached a state no payment event may
// leave. A paid order is the end of this flow; cancelled and expired orders
// are reachable only by paths that do not exist yet, and are treated as final
// here so a future one cannot be regressed by a late event.
func (o Order) Settled() bool {
	return o.Status != StatusPending
}

// MarkPaid moves a pending order to paid.
//
// It reports whether the order actually moves, so the caller can tell a real
// transition from an order that was already paid. The distinction matters
// because the consumer treats "no row updated" during an applied transition as
// a violated invariant, and must not attempt the update at all when the order
// is already where the event wants it.
func (o Order) MarkPaid(now time.Time) (Order, bool) {
	if o.Status != StatusPending {
		return o, false
	}
	o.Status = StatusPaid
	o.UpdatedAt = now.UTC()
	return o, true
}
