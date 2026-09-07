// Package models holds the GORM persistence representations. They are not
// domain entities and are converted at the adapter boundary.
package models

import (
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
)

// Order maps the orders table. The schema is owned by the migrations; GORM
// never migrates it.
type Order struct {
	ID             string    `gorm:"primaryKey"`
	Status         string    `gorm:"not null"`
	Amount         int64     `gorm:"not null"`
	Currency       string    `gorm:"not null"`
	ProductID      string    `gorm:"not null"`
	Quantity       int32     `gorm:"not null"`
	IdempotencyKey string    `gorm:"not null"`
	CreatedAt      time.Time `gorm:"not null"`
	UpdatedAt      time.Time `gorm:"not null"`
}

// TableName pins the table so GORM's pluralisation never matters.
func (Order) TableName() string { return "orders" }

// OrderFromDomain converts an order for persistence.
func OrderFromDomain(order domain.Order) Order {
	return Order{
		ID:             order.ID,
		Status:         string(order.Status),
		Amount:         order.Amount,
		Currency:       string(order.Currency),
		ProductID:      order.ProductID,
		Quantity:       int32(order.Quantity), //nolint:gosec // Quantity is bounded by domain.MaxQuantity.
		IdempotencyKey: order.IdempotencyKey,
		CreatedAt:      order.CreatedAt.UTC(),
		UpdatedAt:      order.UpdatedAt.UTC(),
	}
}

// ToDomain converts a persisted row back into the domain entity.
func (o Order) ToDomain() domain.Order {
	return domain.Order{
		ID:             o.ID,
		Status:         domain.Status(o.Status),
		Amount:         o.Amount,
		Currency:       domain.Currency(o.Currency),
		ProductID:      o.ProductID,
		Quantity:       int(o.Quantity),
		IdempotencyKey: o.IdempotencyKey,
		CreatedAt:      o.CreatedAt.UTC(),
		UpdatedAt:      o.UpdatedAt.UTC(),
	}
}
