// Package repositories implements the persistence ports of the use cases.
package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rmotti/payments-boilerplate/internal/adapters/postgres/models"
	app "github.com/rmotti/payments-boilerplate/internal/application/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	"gorm.io/gorm"
)

const (
	// writeTimeout bounds every statement so a stalled database never pins a
	// request for longer than the caller would tolerate.
	writeTimeout = 5 * time.Second

	uniqueViolation                = "23505"
	ordersIdempotencyKeyConstraint = "orders_idempotency_key_key"
)

// OrderRepository persists orders with GORM. Plain inserts are ordinary CRUD;
// the idempotency guarantee itself lives in the unique index of the schema.
type OrderRepository struct {
	db *gorm.DB
}

// NewOrderRepository binds the repository to a GORM handle.
func NewOrderRepository(db *gorm.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

// Create inserts a new order. A reused idempotency key surfaces as
// app.ErrIdempotencyKeyConflict so the use case can answer deterministically.
func (r *OrderRepository) Create(ctx context.Context, order domain.Order) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	row := models.OrderFromDomain(order)
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isUniqueViolation(err, ordersIdempotencyKeyConstraint) {
			return app.ErrIdempotencyKeyConflict
		}
		return fmt.Errorf("insert order: %w", err)
	}
	return nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}
