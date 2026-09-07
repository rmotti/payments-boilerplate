// Package repositories implements the persistence ports of the use cases.
package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/adapters/postgres/models"
	app "github.com/rmotti/payments-boilerplate/internal/application/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// writeTimeout bounds every statement so a stalled database never pins a
	// request for longer than the caller would tolerate.
	writeTimeout = 5 * time.Second
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

// CreateOrGet inserts an order or returns the row that already owns its
// idempotency key. The application decides whether that replay is equivalent.
func (r *OrderRepository) CreateOrGet(ctx context.Context, order domain.Order) (domain.Order, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	row := models.OrderFromDomain(order)
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "idempotency_key"}},
		DoNothing: true,
	}).Create(&row)
	if result.Error != nil {
		return domain.Order{}, false, fmt.Errorf("insert order: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return order, true, nil
	}

	var existing models.Order
	if err := r.db.WithContext(ctx).Where("idempotency_key = ?", order.IdempotencyKey).First(&existing).Error; err != nil {
		return domain.Order{}, false, fmt.Errorf("find order idempotency replay: %w", err)
	}
	return existing.ToDomain(), false, nil
}

// Get returns an order by its opaque public identifier.
func (r *OrderRepository) Get(ctx context.Context, id string) (domain.Order, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	var row models.Order
	if err := r.db.WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.Order{}, app.ErrOrderNotFound
		}
		return domain.Order{}, fmt.Errorf("select order: %w", err)
	}
	return row.ToDomain(), nil
}
