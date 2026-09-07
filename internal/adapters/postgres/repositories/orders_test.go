package repositories

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
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
	persisted, created, err := repository.CreateOrGet(ctx, order)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !created || persisted != order {
		t.Fatalf("CreateOrGet() = %#v, %v; want created original order", persisted, created)
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
	persisted, created, err = repository.CreateOrGet(ctx, duplicate)
	if err != nil {
		t.Fatalf("CreateOrGet() replay error = %v", err)
	}
	if created || persisted.ID != order.ID {
		t.Fatalf("CreateOrGet() replay = %#v, %v; want original order", persisted, created)
	}

	found, err := repository.Get(ctx, order.ID)
	if err != nil || found != order {
		t.Fatalf("Get() = %#v, %v; want %#v", found, err, order)
	}
}
