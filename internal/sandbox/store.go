package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Register the PostgreSQL database/sql driver.
)

// Store reads sandbox correlation and state directly from the local database.
// Application state changes still enter through the public webhook API.
type Store struct {
	db *sql.DB
}

// AggregateStatus is the persisted end-to-end state shown by sandbox commands.
type AggregateStatus struct {
	OrderID       string `json:"orderId"`
	OrderStatus   string `json:"orderStatus"`
	PaymentID     string `json:"paymentId"`
	PaymentStatus string `json:"paymentStatus"`
	AttemptID     string `json:"attemptId"`
	AttemptStatus string `json:"attemptStatus"`
	ProviderEvent string `json:"providerEventId,omitempty"`
	WebhookStatus string `json:"webhookStatus,omitempty"`
	OutboxStatus  string `json:"outboxStatus,omitempty"`
}

// CleanupResult reports the local rows removed for one explicit sandbox order.
type CleanupResult struct {
	OrderID       string `json:"orderId"`
	Orders        int64  `json:"orders"`
	Payments      int64  `json:"payments"`
	Attempts      int64  `json:"attempts"`
	WebhookEvents int64  `json:"webhookEvents"`
	OutboxEvents  int64  `json:"outboxEvents"`
}

// OpenStore connects to the already validated local PostgreSQL sandbox.
func OpenStore(ctx context.Context, databaseURL string) (*Store, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open sandbox database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sandbox database: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the sandbox database connection.
func (s *Store) Close() error { return s.db.Close() }

// Reference resolves the latest Checkout Session for an order created by this
// tool. The idempotency marker prevents emitting events for arbitrary orders.
func (s *Store) Reference(ctx context.Context, orderID string) (Reference, error) {
	var reference Reference
	var idempotencyKey string
	var paymentIntent sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT o.id, o.idempotency_key, p.id, a.id, a.provider_session_id,
		       a.provider_payment_intent_id, p.amount, p.currency
		FROM orders o
		JOIN payments p ON p.order_id = o.id
		JOIN payment_attempts a ON a.payment_id = p.id
		WHERE o.id = $1
		ORDER BY a.created_at DESC
		LIMIT 1`, orderID).Scan(
		&reference.OrderID, &idempotencyKey, &reference.PaymentID,
		&reference.AttemptID, &reference.SessionID, &paymentIntent,
		&reference.Amount, &reference.Currency,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Reference{}, fmt.Errorf("sandbox order %q or its checkout was not found", orderID)
	}
	if err != nil {
		return Reference{}, fmt.Errorf("read sandbox checkout reference: %w", err)
	}
	if !strings.HasPrefix(idempotencyKey, "sandbox_order_") {
		return Reference{}, fmt.Errorf("order %q was not created by the sandbox tool", orderID)
	}
	if paymentIntent.Valid {
		reference.PaymentIntentID = paymentIntent.String
	}
	return reference, nil
}

// Status reads the latest payment attempt and, when supplied, one emitted
// provider event and its outbox row.
func (s *Store) Status(ctx context.Context, orderID, providerEventID string) (AggregateStatus, error) {
	var status AggregateStatus
	err := s.db.QueryRowContext(ctx, `
		SELECT o.id, o.status, p.id, p.status, a.id, a.status
		FROM orders o
		JOIN payments p ON p.order_id = o.id
		JOIN payment_attempts a ON a.payment_id = p.id
		WHERE o.id = $1
		ORDER BY a.created_at DESC
		LIMIT 1`, orderID).Scan(
		&status.OrderID, &status.OrderStatus, &status.PaymentID,
		&status.PaymentStatus, &status.AttemptID, &status.AttemptStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AggregateStatus{}, fmt.Errorf("sandbox order %q was not found", orderID)
	}
	if err != nil {
		return AggregateStatus{}, fmt.Errorf("read sandbox aggregate: %w", err)
	}
	if providerEventID == "" {
		return status, nil
	}

	status.ProviderEvent = providerEventID
	err = s.db.QueryRowContext(ctx, `
		SELECT w.status, coalesce(ob.status, '')
		FROM webhook_events w
		LEFT JOIN outbox_events ob ON ob.webhook_event_id = w.id
		WHERE w.provider = 'stripe' AND w.provider_event_id = $1`, providerEventID).
		Scan(&status.WebhookStatus, &status.OutboxStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return AggregateStatus{}, fmt.Errorf("read sandbox pipeline state: %w", err)
	}
	return status, nil
}

// WaitForScenario polls durable state until the expected event has traversed
// inbox, outbox and consumer, or the caller's deadline expires.
func (s *Store) WaitForScenario(
	ctx context.Context,
	orderID, providerEventID string,
	scenario Scenario,
) (AggregateStatus, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last AggregateStatus
	for {
		status, err := s.Status(ctx, orderID, providerEventID)
		if err != nil {
			return AggregateStatus{}, err
		}
		last = status
		if scenario.matches(status) {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("wait for %s: %w", scenario.Name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// WaitForOrder polls the public commercial state until it reaches the requested
// value. It is useful after completing a hosted Checkout manually.
func WaitForOrder(ctx context.Context, client *Client, orderID, expected string) (Order, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last Order
	for {
		order, err := client.GetOrder(ctx, orderID)
		if err != nil {
			return Order{}, err
		}
		last = order
		if order.Status == expected {
			return order, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("wait for order status %q: %w", expected, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s Scenario) matches(status AggregateStatus) bool {
	return status.OrderStatus == s.OrderStatus && status.PaymentStatus == s.PaymentStatus &&
		status.AttemptStatus == s.AttemptStatus && status.WebhookStatus == s.WebhookStatus &&
		status.OutboxStatus == s.OutboxStatus
}

// Cleanup removes only terminal local rows related to one explicit order whose
// idempotency key proves it was created by this tool. Remote Stripe test
// sessions are not deleted and expire according to Stripe's sandbox behavior.
func (s *Store) Cleanup(ctx context.Context, orderID string) (result CleanupResult, err error) {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("begin sandbox cleanup: %w", err)
	}
	defer func() {
		if err != nil {
			_ = transaction.Rollback()
		}
	}()

	var idempotencyKey string
	err = transaction.QueryRowContext(ctx,
		"SELECT idempotency_key FROM orders WHERE id = $1 FOR UPDATE", orderID).
		Scan(&idempotencyKey)
	if errors.Is(err, sql.ErrNoRows) {
		return CleanupResult{}, fmt.Errorf("sandbox order %q was not found", orderID)
	}
	if err != nil {
		return CleanupResult{}, fmt.Errorf("lock sandbox order: %w", err)
	}
	if !strings.HasPrefix(idempotencyKey, "sandbox_order_") {
		return CleanupResult{}, fmt.Errorf("refusing to delete order %q: it was not created by the sandbox tool", orderID)
	}

	var busy int
	err = transaction.QueryRowContext(ctx, `
		SELECT count(*)
		FROM webhook_events w
		LEFT JOIN outbox_events ob ON ob.webhook_event_id = w.id
		WHERE (w.payload #>> '{data,object,metadata,order_id}' = $1
		       OR w.payload #>> '{data,object,client_reference_id}' = $1)
		  AND (w.status IN ('pending', 'processing')
		       OR coalesce(ob.status, 'published') NOT IN ('published', 'failed'))`, orderID).
		Scan(&busy)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("check sandbox work in progress: %w", err)
	}
	if busy != 0 {
		return CleanupResult{}, fmt.Errorf("refusing to delete order %q while %d event(s) are still in progress", orderID, busy)
	}

	result.OrderID = orderID
	if result.OutboxEvents, err = execRows(ctx, transaction, `
		DELETE FROM outbox_events
		WHERE webhook_event_id IN (
			SELECT id FROM webhook_events
			WHERE payload #>> '{data,object,metadata,order_id}' = $1
			   OR payload #>> '{data,object,client_reference_id}' = $1
		)`, orderID); err != nil {
		return CleanupResult{}, err
	}
	if result.WebhookEvents, err = execRows(ctx, transaction, `
		DELETE FROM webhook_events
		WHERE payload #>> '{data,object,metadata,order_id}' = $1
		   OR payload #>> '{data,object,client_reference_id}' = $1`, orderID); err != nil {
		return CleanupResult{}, err
	}
	if result.Attempts, err = execRows(ctx, transaction, `
		DELETE FROM payment_attempts
		WHERE payment_id IN (SELECT id FROM payments WHERE order_id = $1)`, orderID); err != nil {
		return CleanupResult{}, err
	}
	if result.Payments, err = execRows(ctx, transaction,
		"DELETE FROM payments WHERE order_id = $1", orderID); err != nil {
		return CleanupResult{}, err
	}
	if result.Orders, err = execRows(ctx, transaction,
		"DELETE FROM orders WHERE id = $1", orderID); err != nil {
		return CleanupResult{}, err
	}
	if err = transaction.Commit(); err != nil {
		return CleanupResult{}, fmt.Errorf("commit sandbox cleanup: %w", err)
	}
	return result, nil
}

func execRows(ctx context.Context, transaction *sql.Tx, query, orderID string) (int64, error) {
	result, err := transaction.ExecContext(ctx, query, orderID)
	if err != nil {
		return 0, fmt.Errorf("delete sandbox rows: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted sandbox rows: %w", err)
	}
	return rows, nil
}
