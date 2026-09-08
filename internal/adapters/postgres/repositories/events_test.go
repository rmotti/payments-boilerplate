package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	app "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	payments "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/database"
	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
)

// openConsumerTestDatabase skips the test when no database is configured, and
// otherwise returns a migrated handle.
func openConsumerTestDatabase(t *testing.T) *database.Database {
	t.Helper()

	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}
	return openMigratedTestDatabase(t, databaseURL)
}

// seedAggregate creates one order, payment and attempt, and returns their ids.
func seedAggregate(t *testing.T, db *database.Database, suffix string) (orderID, paymentID, attemptID string) {
	t.Helper()

	orderID = "ord_consumer_" + suffix
	paymentID = "pay_consumer_" + suffix
	attemptID = "pat_consumer_" + suffix
	ctx := context.Background()

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO orders (id,status,amount,currency,product_id,quantity,idempotency_key)
		  VALUES ($1,'pending',10000,'BRL','product_x',1,$2)`, []any{orderID, "idem_" + suffix}},
		{`INSERT INTO payments (id,order_id,provider,status,amount,currency)
		  VALUES ($1,$2,'stripe','pending',10000,'BRL')`, []any{paymentID, orderID}},
		{`INSERT INTO payment_attempts (id,payment_id,provider,status,idempotency_key,provider_session_id)
		  VALUES ($1,$2,'stripe','pending',$3,$4)`, []any{attemptID, paymentID, "idem_att_" + suffix, "cs_" + suffix}},
	}
	for _, statement := range statements {
		if _, err := db.SQL.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed aggregate: %v", err)
		}
	}

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(ctx, "DELETE FROM payment_attempts WHERE payment_id = $1", paymentID)
		_, _ = db.SQL.ExecContext(ctx, "DELETE FROM payments WHERE id = $1", paymentID)
		_, _ = db.SQL.ExecContext(ctx, "DELETE FROM orders WHERE id = $1", orderID)
	})
	return orderID, paymentID, attemptID
}

// seedInboxEvent stores one pending inbox entry without an outbox message.
func seedInboxEvent(t *testing.T, db *database.Database, suffix string) string {
	t.Helper()

	eventID, _ := domain.NewEventID()
	providerEventID := "evt_consumer_" + suffix + "_" + eventID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)
	now := time.Now().UTC().Truncate(time.Microsecond)

	event, err := domain.NewEvent(eventID, domain.Stripe, providerEventID,
		"checkout.session.completed", domain.KindCheckoutCompleted, raw, json.RawMessage(raw), now)
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	if _, err := NewWebhookRepository(db.SQL).StoreIgnored(context.Background(), event); err != nil {
		t.Fatalf("seed inbox event: %v", err)
	}
	// StoreIgnored writes it as skipped; the consumer only ever sees pending.
	if _, err := db.SQL.ExecContext(context.Background(),
		"UPDATE webhook_events SET status = 'pending' WHERE id = $1", eventID); err != nil {
		t.Fatalf("reset seeded event status: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(), "DELETE FROM webhook_events WHERE id = $1", eventID)
	})
	return eventID
}

func aggregateState(t *testing.T, db *database.Database, orderID, paymentID, attemptID string) (order, payment, attempt string) {
	t.Helper()
	ctx := context.Background()
	if err := db.SQL.QueryRowContext(ctx, "SELECT status FROM orders WHERE id = $1", orderID).Scan(&order); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if err := db.SQL.QueryRowContext(ctx, "SELECT status FROM payments WHERE id = $1", paymentID).Scan(&payment); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if err := db.SQL.QueryRowContext(ctx, "SELECT status FROM payment_attempts WHERE id = $1", attemptID).Scan(&attempt); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	return order, payment, attempt
}

func eventState(t *testing.T, db *database.Database, eventID string) (status string, attempts int) {
	t.Helper()
	if err := db.SQL.QueryRowContext(context.Background(),
		"SELECT status, attempts FROM webhook_events WHERE id = $1", eventID).Scan(&status, &attempts); err != nil {
		t.Fatalf("read event state: %v", err)
	}
	return status, attempts
}

// resolveTo builds a resolver that always names the same aggregate.
func resolveTo(orderID, paymentID, attemptID string) app.Resolver {
	return func(app.InboxEntry) (app.Reference, error) {
		return app.Reference{OrderID: orderID, PaymentID: paymentID, AttemptID: attemptID}, nil
	}
}

func TestEventRepositoryAppliesTransitionsAtomically(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	orderID, paymentID, attemptID := seedAggregate(t, db, "apply")
	eventID := seedInboxEvent(t, db, "apply")

	result, err := repository.Process(context.Background(), eventID,
		resolveTo(orderID, paymentID, attemptID),
		func(_ app.InboxEntry, _ app.Aggregate) (app.Effect, error) {
			return app.Effect{
				AttemptStatus: payments.AttemptStatusSucceeded,
				PaymentStatus: payments.StatusSucceeded,
				OrderPaid:     true,
			}, nil
		})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if !result.Applied {
		t.Error("Applied = false, want a recorded transition")
	}

	order, payment, attempt := aggregateState(t, db, orderID, paymentID, attemptID)
	if order != "paid" || payment != "succeeded" || attempt != "succeeded" {
		t.Errorf("state = %s/%s/%s, want paid/succeeded/succeeded", order, payment, attempt)
	}
	if status, attempts := eventState(t, db, eventID); status != "processed" || attempts != 1 {
		t.Errorf("event = %s with %d attempts, want processed with 1", status, attempts)
	}
}

// The effect and the inbox status must commit together. If the decider fails
// after the aggregate is locked, nothing at all may be written.
func TestEventRepositoryRollsBackEverythingOnFailure(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	orderID, paymentID, attemptID := seedAggregate(t, db, "rollback")
	eventID := seedInboxEvent(t, db, "rollback")

	wantErr := errors.New("decider refused")
	_, err := repository.Process(context.Background(), eventID,
		resolveTo(orderID, paymentID, attemptID),
		func(_ app.InboxEntry, _ app.Aggregate) (app.Effect, error) {
			return app.Effect{}, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Process() error = %v, want %v", err, wantErr)
	}

	order, payment, attempt := aggregateState(t, db, orderID, paymentID, attemptID)
	if order != "pending" || payment != "pending" || attempt != "pending" {
		t.Errorf("state = %s/%s/%s, want everything still pending", order, payment, attempt)
	}
	if status, attempts := eventState(t, db, eventID); status != "pending" || attempts != 0 {
		t.Errorf("event = %s with %d attempts, want an untouched pending", status, attempts)
	}
}

// A redelivery must find the entry processed and do nothing, without even
// reaching the aggregate.
func TestEventRepositoryTreatsAProcessedEventAsANoOp(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	orderID, paymentID, attemptID := seedAggregate(t, db, "redelivery")
	eventID := seedInboxEvent(t, db, "redelivery")

	apply := func(_ app.InboxEntry, _ app.Aggregate) (app.Effect, error) {
		return app.Effect{PaymentStatus: payments.StatusProcessing}, nil
	}
	if _, err := repository.Process(context.Background(), eventID,
		resolveTo(orderID, paymentID, attemptID), apply); err != nil {
		t.Fatalf("first Process() error = %v", err)
	}

	result, err := repository.Process(context.Background(), eventID,
		func(app.InboxEntry) (app.Reference, error) {
			t.Error("a processed event must not be resolved again")
			return app.Reference{}, errors.New("must not be called")
		},
		func(_ app.InboxEntry, _ app.Aggregate) (app.Effect, error) {
			t.Error("a processed event must not be decided again")
			return app.Effect{}, errors.New("must not be called")
		})
	if err != nil {
		t.Fatalf("second Process() error = %v", err)
	}
	if result.Applied {
		t.Error("Applied = true, want a no-op on redelivery")
	}

	if _, _, attempt := aggregateState(t, db, orderID, paymentID, attemptID); attempt != "pending" {
		t.Errorf("attempt = %s, want it untouched by the redelivery", attempt)
	}
}

// The reason the lock covers the whole aggregate: two different events of the
// same payment must not interleave. Without the payment row lock both would
// read pending and both would try to settle it.
func TestEventRepositorySerializesDifferentEventsOfOnePayment(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	orderID, paymentID, attemptID := seedAggregate(t, db, "concurrent")
	firstEvent := seedInboxEvent(t, db, "concurrent_a")
	secondEvent := seedInboxEvent(t, db, "concurrent_b")

	// Both deciders read the state they were given and try to settle the
	// payment. Exactly one may succeed; the other must observe the settled
	// state and decline, which is what proves they did not interleave.
	var mu sync.Mutex
	var observed []payments.Status

	settle := func(_ app.InboxEntry, aggregate app.Aggregate) (app.Effect, error) {
		mu.Lock()
		observed = append(observed, aggregate.PaymentStatus)
		mu.Unlock()

		if aggregate.PaymentStatus != payments.StatusPending {
			return app.Effect{Note: "already settled"}, nil
		}
		return app.Effect{
			AttemptStatus: payments.AttemptStatusSucceeded,
			PaymentStatus: payments.StatusSucceeded,
			OrderPaid:     true,
		}, nil
	}

	var wait sync.WaitGroup
	errs := make([]error, 2)
	for i, eventID := range []string{firstEvent, secondEvent} {
		wait.Add(1)
		go func(index int, id string) {
			defer wait.Done()
			_, errs[index] = repository.Process(context.Background(), id,
				resolveTo(orderID, paymentID, attemptID), settle)
		}(i, eventID)
	}
	wait.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Process() %d error = %v", i, err)
		}
	}

	// One transaction saw pending; the other must have seen the committed
	// result of the first. Two pendings would mean the lock did not hold.
	settled := 0
	for _, status := range observed {
		if status != payments.StatusPending {
			settled++
		}
	}
	if settled != 1 {
		t.Errorf("observed payment states = %v, want exactly one to see a settled payment", observed)
	}

	order, payment, attempt := aggregateState(t, db, orderID, paymentID, attemptID)
	if order != "paid" || payment != "succeeded" || attempt != "succeeded" {
		t.Errorf("state = %s/%s/%s, want a single applied settlement", order, payment, attempt)
	}
}

// An approved transition that changes no row means the matrix and the schema
// disagree. It must be reported, not silently accepted as a no-op.
func TestEventRepositoryReportsATransitionThatChangesNothing(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	orderID, paymentID, attemptID := seedAggregate(t, db, "invariant")
	eventID := seedInboxEvent(t, db, "invariant")

	if _, err := db.SQL.ExecContext(context.Background(),
		"UPDATE payments SET status = 'failed' WHERE id = $1", paymentID); err != nil {
		t.Fatalf("settle payment: %v", err)
	}

	_, err := repository.Process(context.Background(), eventID,
		resolveTo(orderID, paymentID, attemptID),
		func(_ app.InboxEntry, _ app.Aggregate) (app.Effect, error) {
			// The schema refuses to move a failed payment, so this update
			// matches zero rows.
			return app.Effect{PaymentStatus: payments.StatusSucceeded}, nil
		})
	if !errors.Is(err, app.ErrInvariantViolated) {
		t.Fatalf("Process() error = %v, want %v", err, app.ErrInvariantViolated)
	}
}

func TestEventRepositoryRefusesToRegressARefundedPayment(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	orderID, paymentID, attemptID := seedAggregate(t, db, "refunded_invariant")
	eventID := seedInboxEvent(t, db, "refunded_invariant")

	if _, err := db.SQL.ExecContext(context.Background(),
		"UPDATE payments SET status = 'refunded' WHERE id = $1", paymentID); err != nil {
		t.Fatalf("refund payment: %v", err)
	}

	_, err := repository.Process(context.Background(), eventID,
		resolveTo(orderID, paymentID, attemptID),
		func(_ app.InboxEntry, _ app.Aggregate) (app.Effect, error) {
			return app.Effect{PaymentStatus: payments.StatusProcessing}, nil
		})
	if !errors.Is(err, app.ErrInvariantViolated) {
		t.Fatalf("Process() error = %v, want %v", err, app.ErrInvariantViolated)
	}

	if _, payment, _ := aggregateState(t, db, orderID, paymentID, attemptID); payment != "refunded" {
		t.Errorf("payment = %s, want refunded after the refused regression", payment)
	}
}

// A reference whose parts do not belong together must be refused, not applied
// to whichever row happens to exist.
func TestEventRepositoryRefusesAnInconsistentReference(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	orderID, paymentID, attemptID := seedAggregate(t, db, "mismatch_a")
	otherOrder, _, _ := seedAggregate(t, db, "mismatch_b")
	eventID := seedInboxEvent(t, db, "mismatch")

	_ = orderID
	_, err := repository.Process(context.Background(), eventID,
		// The attempt and payment are real, but the order belongs to another
		// aggregate entirely.
		resolveTo(otherOrder, paymentID, attemptID),
		func(_ app.InboxEntry, _ app.Aggregate) (app.Effect, error) {
			t.Error("an inconsistent reference must never reach the decider")
			return app.Effect{}, nil
		})
	if !errors.Is(err, app.ErrReferenceMismatch) {
		t.Fatalf("Process() error = %v, want %v", err, app.ErrReferenceMismatch)
	}
}

func TestEventRepositoryRecordsFailures(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	eventID := seedInboxEvent(t, db, "failures")
	ctx := context.Background()

	attempts, failed, err := repository.RecordFailure(
		ctx, eventID, fmt.Errorf("broker unreachable"), false, 3)
	if err != nil {
		t.Fatalf("RecordFailure() error = %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if failed {
		t.Error("failed = true, want a transient failure below budget to stay pending")
	}
	if status, _ := eventState(t, db, eventID); status != "pending" {
		t.Errorf("status = %s, want a transient failure to leave it pending", status)
	}

	attempts, failed, err = repository.RecordFailure(
		ctx, eventID, fmt.Errorf("payload is broken"), true, 3)
	if err != nil {
		t.Fatalf("terminal RecordFailure() error = %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if !failed {
		t.Error("failed = false, want a terminal failure to close the entry")
	}
	if status, _ := eventState(t, db, eventID); status != "failed" {
		t.Errorf("status = %s, want a terminal failure to close the entry", status)
	}
}

func TestEventRepositoryClosesAnEntryWhenTheRetryBudgetIsSpent(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)

	eventID := seedInboxEvent(t, db, "budget")
	ctx := context.Background()

	for want := 1; want <= 3; want++ {
		attempts, failed, err := repository.RecordFailure(
			ctx, eventID, fmt.Errorf("temporary database failure"), false, 3)
		if err != nil {
			t.Fatalf("RecordFailure() attempt %d error = %v", want, err)
		}
		if attempts != want {
			t.Fatalf("attempts = %d, want %d", attempts, want)
		}
		if failed != (want == 3) {
			t.Fatalf("failed = %t on attempt %d, want %t", failed, want, want == 3)
		}
	}

	if status, attempts := eventState(t, db, eventID); status != "failed" || attempts != 3 {
		t.Errorf("event = %s with %d attempts, want failed with 3", status, attempts)
	}
}

// TestEventRepositoryRecordFailureSanitizesLastError guards the E8b invariant
// that RecordFailure never persists a cause verbatim: the inbox last_error
// column is returned by the operational API, so a credential that reaches
// err.Error() here must not survive into the row. The sentinels are unique so
// a leak cannot be confused with anything a real failure would legitimately
// contain.
func TestEventRepositoryRecordFailureSanitizesLastError(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewEventRepository(db.SQL)
	ctx := context.Background()

	const sentinel = "sentinel-inbox-EAF3B9"
	cause := fmt.Errorf(
		"dial failed: postgres://payments:%s@db.internal:5432/payments and Authorization: Bearer %s",
		sentinel, sentinel,
	)

	eventID := seedInboxEvent(t, db, "sanitize")
	if _, _, err := repository.RecordFailure(ctx, eventID, cause, false, 3); err != nil {
		t.Fatalf("RecordFailure() error = %v", err)
	}

	var lastError string
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT last_error FROM webhook_events WHERE id = $1", eventID).Scan(&lastError); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if strings.Contains(lastError, sentinel) {
		t.Fatalf("last_error = %q, leaked the sentinel credential", lastError)
	}
	if !strings.Contains(lastError, errsanitize.Redacted) {
		t.Fatalf("last_error = %q, want the stable redaction marker", lastError)
	}
}
