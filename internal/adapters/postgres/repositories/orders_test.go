package repositories

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	app "github.com/rmotti/payments-boilerplate/internal/application/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/database"
	"go.uber.org/zap"
)

const testDatabaseURLEnv = "TEST_DATABASE_URL"

func TestOrderRepositoryCreateIntegration(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db, err := database.Open(ctx, config.Config{
		DatabaseURL:                   databaseURL,
		DatabaseMaxOpenConnections:    2,
		DatabaseMaxIdleConnections:    1,
		DatabaseConnectionMaxLifetime: time.Minute,
	}, zap.NewNop())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	migrationsDir, err := filepath.Abs("../../../../db/migrations")
	if err != nil {
		t.Fatalf("resolve migrations directory: %v", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("configure migrations: %v", err)
	}
	if err := goose.Up(db.SQL, migrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	orderID, err := domain.NewID()
	if err != nil {
		t.Fatalf("generate order id: %v", err)
	}
	secondOrderID, err := domain.NewID()
	if err != nil {
		t.Fatalf("generate second order id: %v", err)
	}
	idempotencyKey := "integration-" + orderID
	t.Cleanup(func() {
		if _, err := db.SQL.ExecContext(context.Background(), "DELETE FROM orders WHERE id = $1", orderID); err != nil {
			t.Errorf("delete test order: %v", err)
		}
	})

	now := time.Now().UTC().Truncate(time.Microsecond)
	order, err := domain.New(orderID, domain.Product{
		ID:         "product_integration",
		UnitAmount: 1250,
		Currency:   domain.BRL,
	}, 3, idempotencyKey, now)
	if err != nil {
		t.Fatalf("create domain order: %v", err)
	}

	repository := NewOrderRepository(db.GORM)
	if err := repository.Create(ctx, order); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var amount int64
	var currency, productID, status string
	var quantity int
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT amount, currency, product_id, quantity, status
		FROM orders
		WHERE id = $1
	`, orderID).Scan(&amount, &currency, &productID, &quantity, &status); err != nil {
		t.Fatalf("read persisted order: %v", err)
	}
	if amount != 3750 || currency != "BRL" || productID != "product_integration" || quantity != 3 || status != "pending" {
		t.Fatalf(
			"persisted order = %d %s, product %q x%d, status %q; want 3750 BRL, product_integration x3, pending",
			amount, currency, productID, quantity, status,
		)
	}

	duplicate, err := domain.New(secondOrderID, domain.Product{
		ID:         "product_integration",
		UnitAmount: 1250,
		Currency:   domain.BRL,
	}, 3, idempotencyKey, now)
	if err != nil {
		t.Fatalf("create duplicate domain order: %v", err)
	}
	if err := repository.Create(ctx, duplicate); !errors.Is(err, app.ErrIdempotencyKeyConflict) {
		t.Fatalf("Create() duplicate error = %v, want %v", err, app.ErrIdempotencyKeyConflict)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	t.Parallel()

	conflict := &pgconn.PgError{Code: "23505", ConstraintName: "orders_idempotency_key_key"}
	if !isUniqueViolation(fmt.Errorf("wrapped: %w", conflict), "orders_idempotency_key_key") {
		t.Fatal("isUniqueViolation() = false for a wrapped orders_idempotency_key_key violation")
	}
	if isUniqueViolation(&pgconn.PgError{Code: "23505", ConstraintName: "orders_pkey"}, "orders_idempotency_key_key") {
		t.Fatal("isUniqueViolation() = true for a different constraint")
	}
	if isUniqueViolation(&pgconn.PgError{Code: "23503", ConstraintName: "orders_idempotency_key_key"}, "orders_idempotency_key_key") {
		t.Fatal("isUniqueViolation() = true for a non-unique error code")
	}
	if isUniqueViolation(errors.New("plain"), "orders_idempotency_key_key") {
		t.Fatal("isUniqueViolation() = true for a plain error")
	}
}
