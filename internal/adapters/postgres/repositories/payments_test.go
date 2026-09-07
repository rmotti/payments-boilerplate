package repositories

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	app "github.com/rmotti/payments-boilerplate/internal/application/payments"
	orders "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/database"
	"go.uber.org/zap"
)

func TestPaymentRepositoryCheckoutIntegration(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db, err := database.Open(ctx, config.Config{
		DatabaseURL: databaseURL, DatabaseMaxOpenConnections: 4,
		DatabaseMaxIdleConnections: 1, DatabaseConnectionMaxLifetime: time.Minute,
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
		t.Fatalf("resolve migrations: %v", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("configure migrations: %v", err)
	}
	if err := goose.Up(db.SQL, migrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	orderID, _ := orders.NewID()
	otherOrderID, _ := orders.NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	order, err := orders.New(orderID, orders.Product{
		ID: "product_integration", UnitAmount: 1250, Currency: orders.BRL,
	}, 2, "order-"+orderID, now)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	otherOrder, err := orders.New(otherOrderID, orders.Product{
		ID: "product_integration", UnitAmount: 1250, Currency: orders.BRL,
	}, 2, "order-"+otherOrderID, now)
	if err != nil {
		t.Fatalf("create other order: %v", err)
	}
	orderRepository := NewOrderRepository(db.GORM)
	if _, _, err := orderRepository.CreateOrGet(ctx, order); err != nil {
		t.Fatalf("persist order: %v", err)
	}
	if _, _, err := orderRepository.CreateOrGet(ctx, otherOrder); err != nil {
		t.Fatalf("persist other order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(), "DELETE FROM payment_attempts WHERE payment_id IN (SELECT id FROM payments WHERE order_id IN ($1, $2))", orderID, otherOrderID)
		_, _ = db.SQL.ExecContext(context.Background(), "DELETE FROM payments WHERE order_id IN ($1, $2)", orderID, otherOrderID)
		_, _ = db.SQL.ExecContext(context.Background(), "DELETE FROM orders WHERE id IN ($1, $2)", orderID, otherOrderID)
	})

	payment, attempt, err := domain.New("pay_integration_1", "pat_integration_1", order, "checkout-"+orderID, now)
	if err != nil {
		t.Fatalf("create payment aggregate: %v", err)
	}
	repository := NewPaymentRepository(db.GORM)
	preparedPayment, preparedAttempt, err := repository.PrepareCheckout(ctx, payment, attempt)
	if err != nil {
		t.Fatalf("PrepareCheckout() error = %v", err)
	}
	if preparedPayment.ID != payment.ID || preparedAttempt.ID != attempt.ID || preparedAttempt.Status != domain.AttemptStatusCreated {
		t.Fatalf("prepared = %#v/%#v, want proposed payment and attempt", preparedPayment, preparedAttempt)
	}

	session := domain.Session{
		ID: "cs_test_integration", PaymentIntentID: "pi_test_integration",
		URL: "https://checkout.stripe.com/c/pay/integration", ExpiresAt: now.Add(24 * time.Hour),
	}
	preparedAttempt, err = preparedAttempt.AttachSession(session, now.Add(time.Second))
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}
	if err := repository.SaveSession(ctx, preparedAttempt); err != nil {
		t.Fatalf("SaveSession() error = %v", err)
	}
	if err := repository.SaveSession(ctx, preparedAttempt); err != nil {
		t.Fatalf("SaveSession() replay error = %v", err)
	}

	replayPayment, replayAttempt, err := domain.New("pay_integration_2", "pat_integration_2", order, attempt.IdempotencyKey, now)
	if err != nil {
		t.Fatalf("create replay aggregate: %v", err)
	}
	replayPayment, replayAttempt, err = repository.PrepareCheckout(ctx, replayPayment, replayAttempt)
	if err != nil {
		t.Fatalf("PrepareCheckout() replay error = %v", err)
	}
	if replayPayment.ID != payment.ID || replayAttempt.ProviderSessionID != session.ID || !replayAttempt.HasSession() {
		t.Fatalf("replay = %#v/%#v, want original persisted session", replayPayment, replayAttempt)
	}

	differentPayment, differentAttempt, err := domain.New("pay_integration_3", "pat_integration_3", order, "different-"+orderID, now)
	if err != nil {
		t.Fatalf("create competing aggregate: %v", err)
	}
	if _, _, err := repository.PrepareCheckout(ctx, differentPayment, differentAttempt); !errors.Is(err, app.ErrCheckoutInProgress) {
		t.Fatalf("PrepareCheckout() competing error = %v, want %v", err, app.ErrCheckoutInProgress)
	}

	foreignPayment, foreignAttempt, err := domain.New("pay_integration_4", "pat_integration_4", otherOrder, attempt.IdempotencyKey, now)
	if err != nil {
		t.Fatalf("create foreign aggregate: %v", err)
	}
	if _, _, err := repository.PrepareCheckout(ctx, foreignPayment, foreignAttempt); !errors.Is(err, app.ErrIdempotencyKeyConflict) {
		t.Fatalf("PrepareCheckout() foreign replay error = %v, want %v", err, app.ErrIdempotencyKeyConflict)
	}

	// Two different idempotency keys racing for a fresh order cannot both
	// reserve a chargeable attempt. PostgreSQL's partial indexes serialize the
	// conflict even though each request uses its own transaction.
	type result struct{ err error }
	results := make(chan result, 2)
	for index, key := range []string{"race-a-" + otherOrderID, "race-b-" + otherOrderID} {
		paymentID := fmt.Sprintf("pay_race_%d", index)
		attemptID := fmt.Sprintf("pat_race_%d", index)
		go func() {
			candidatePayment, candidateAttempt, newErr := domain.New(paymentID, attemptID, otherOrder, key, now)
			if newErr == nil {
				_, _, newErr = repository.PrepareCheckout(ctx, candidatePayment, candidateAttempt)
			}
			results <- result{err: newErr}
		}()
	}
	successes, conflicts := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			successes++
		case errors.Is(result.err, app.ErrCheckoutInProgress):
			conflicts++
		default:
			t.Fatalf("concurrent PrepareCheckout() error = %v", result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results = %d success/%d conflict, want 1/1", successes, conflicts)
	}
}
