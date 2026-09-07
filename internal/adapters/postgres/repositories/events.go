package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	dbgen "github.com/rmotti/payments-boilerplate/internal/adapters/postgres/queries"
	app "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	payments "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

// processTimeout bounds the whole applying transaction. It is short because
// the transaction holds locks on a payment aggregate and performs no network
// I/O: everything it needs was already persisted by the API.
const processTimeout = 10 * time.Second

// EventRepository applies the effects of a received event.
//
// Every statement is sqlc over database/sql, in a single transaction. GORM
// does not participate: the shape of these queries is the guarantee — the
// aggregate lock, the conditional transitions and their row counts — which is
// exactly what ADR 0007 reserves for explicit SQL.
type EventRepository struct {
	db      *sql.DB
	queries *dbgen.Queries
}

// NewEventRepository binds the repository to the shared pool.
func NewEventRepository(db *sql.DB) *EventRepository {
	return &EventRepository{db: db, queries: dbgen.New(db)}
}

// Process runs decide against locked state and writes whatever it returns.
//
// The lock order is fixed — inbox entry first, then attempt, payment and order
// together — because two transactions taking the same locks in different
// orders deadlock. Nothing between BEGIN and COMMIT touches the network.
func (r *EventRepository) Process(
	ctx context.Context,
	eventID string,
	resolve app.Resolver,
	decide app.Decider,
) (app.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return app.Result{}, fmt.Errorf("begin consumer transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	queries := r.queries.WithTx(tx)

	entry, err := lockEntry(ctx, queries, eventID)
	if err != nil {
		return app.Result{}, err
	}
	// An entry already closed is the redelivery case: the first arrival
	// committed both the effect and this status, so there is nothing to do
	// and nothing to write.
	if entry.Status == domain.StatusProcessed {
		return app.Result{Applied: false, Note: "event was already processed"}, nil
	}

	aggregate, err := lockAggregate(ctx, queries, entry, resolve)
	if err != nil {
		return app.Result{}, err
	}

	effect, err := decide(entry, aggregate)
	if err != nil {
		return app.Result{}, err
	}

	applied, err := applyEffect(ctx, queries, aggregate, effect)
	if err != nil {
		return app.Result{}, err
	}

	if _, err := queries.MarkWebhookEventProcessed(ctx, dbgen.MarkWebhookEventProcessedParams{
		ID:   eventID,
		Note: nullableText(effect.Note),
	}); err != nil {
		return app.Result{}, fmt.Errorf("mark webhook event processed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return app.Result{}, fmt.Errorf("commit consumer transaction: %w", err)
	}
	return app.Result{Applied: applied, Note: effect.Note}, nil
}

func lockEntry(ctx context.Context, queries *dbgen.Queries, eventID string) (app.InboxEntry, error) {
	row, err := queries.LockWebhookEvent(ctx, eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The outbox row references the inbox row by foreign key, so a
			// message can only name an event that exists. Reaching here means
			// the two disagree, which no retry repairs.
			return app.InboxEntry{}, fmt.Errorf("%w: webhook event %s", app.ErrAggregateNotFound, eventID)
		}
		return app.InboxEntry{}, fmt.Errorf("lock webhook event: %w", err)
	}

	return app.InboxEntry{
		Event: domain.Event{
			ID:              row.ID,
			Provider:        domain.Provider(row.Provider),
			ProviderEventID: row.ProviderEventID,
			Type:            row.EventType,
			// Kind is deliberately left unset. The column stores the
			// provider's own event name, and only the adapter knows how to
			// translate it; inferring a domain kind here would put provider
			// vocabulary in the repository.
			RawPayload: row.RawPayload,
			Payload:    json.RawMessage(row.Payload),
			Status:     domain.Status(row.Status),
			ReceivedAt: row.ReceivedAt.UTC(),
		},
		Attempts: int(row.Attempts),
		Status:   domain.Status(row.Status),
	}, nil
}

// lockAggregate resolves and locks the rows an event acts upon.
//
// The identifiers come from the interpreted payload, so they are attacker-
// influenced only to the extent that the provider's signature allowed. The
// join is what verifies they belong together; a mismatch returns no row.
func lockAggregate(
	ctx context.Context,
	queries *dbgen.Queries,
	entry app.InboxEntry,
	resolve app.Resolver,
) (app.Aggregate, error) {
	reference, err := resolve(entry)
	if err != nil {
		return app.Aggregate{}, err
	}

	row, err := queries.LockPaymentAggregate(ctx, dbgen.LockPaymentAggregateParams{
		AttemptID: reference.AttemptID,
		PaymentID: reference.PaymentID,
		OrderID:   reference.OrderID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Either the rows do not exist, or they exist but do not belong
			// together. Both are terminal, and neither is worth a second
			// query to tell apart inside a locking transaction.
			return app.Aggregate{}, fmt.Errorf(
				"%w: attempt %s, payment %s, order %s",
				app.ErrReferenceMismatch, reference.AttemptID, reference.PaymentID, reference.OrderID)
		}
		return app.Aggregate{}, fmt.Errorf("lock payment aggregate: %w", err)
	}

	aggregate := app.Aggregate{
		OrderID:       row.OrderID,
		OrderStatus:   row.OrderStatus,
		PaymentID:     row.PaymentID,
		PaymentStatus: payments.Status(row.PaymentStatus),
		Provider:      row.Provider,
		Amount:        row.Amount,
		Currency:      row.Currency,
		AttemptID:     row.AttemptID,
		AttemptStatus: payments.AttemptStatus(row.AttemptStatus),
	}
	if row.SessionID.Valid {
		aggregate.SessionID = row.SessionID.String
	}
	return aggregate, nil
}

// applyEffect writes the transitions the decider approved.
//
// Each update must move exactly one row. The decider already read this state
// under the lock and the matrix already approved the move, so zero rows here
// is not a lost race — nothing else can hold these rows — but a disagreement
// between the matrix and the schema. It is reported as a violated invariant
// rather than swallowed as a no-op.
func applyEffect(
	ctx context.Context,
	queries *dbgen.Queries,
	aggregate app.Aggregate,
	effect app.Effect,
) (bool, error) {
	applied := false

	if effect.AttemptStatus != "" {
		affected, err := queries.TransitionPaymentAttempt(ctx, dbgen.TransitionPaymentAttemptParams{
			ID:     aggregate.AttemptID,
			Status: string(effect.AttemptStatus),
		})
		if err != nil {
			return false, fmt.Errorf("transition payment attempt: %w", err)
		}
		if affected != 1 {
			return false, fmt.Errorf("%w: attempt %s to %s matched %d rows",
				app.ErrInvariantViolated, aggregate.AttemptID, effect.AttemptStatus, affected)
		}
		applied = true
	}

	if effect.PaymentStatus != "" {
		affected, err := queries.TransitionPayment(ctx, dbgen.TransitionPaymentParams{
			ID:     aggregate.PaymentID,
			Status: string(effect.PaymentStatus),
		})
		if err != nil {
			return false, fmt.Errorf("transition payment: %w", err)
		}
		if affected != 1 {
			return false, fmt.Errorf("%w: payment %s to %s matched %d rows",
				app.ErrInvariantViolated, aggregate.PaymentID, effect.PaymentStatus, affected)
		}
		applied = true
	}

	if effect.OrderPaid {
		affected, err := queries.MarkOrderPaid(ctx, aggregate.OrderID)
		if err != nil {
			return false, fmt.Errorf("mark order paid: %w", err)
		}
		if affected != 1 {
			return false, fmt.Errorf("%w: order %s to paid matched %d rows",
				app.ErrInvariantViolated, aggregate.OrderID, affected)
		}
		applied = true
	}

	return applied, nil
}

// RecordFailure notes an attempt that did not succeed, in its own short
// transaction. The processing transaction has already rolled back, so this
// must not depend on anything it did.
func (r *EventRepository) RecordFailure(
	ctx context.Context,
	eventID string,
	cause error,
	terminal bool,
	maxAttempts int,
) (int, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	text := errorText(cause)
	if !text.Valid {
		text = sql.NullString{String: "unspecified failure", Valid: true}
	}

	row, err := r.queries.RecordWebhookEventFailure(ctx, dbgen.RecordWebhookEventFailureParams{
		ID: eventID, LastError: text, Terminal: terminal, MaxAttempts: int32(maxAttempts),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A missing row cannot be retried into existence or carry a durable
			// attempt count. Report it as terminal so the caller can explicitly
			// move the orphan message to the DLQ.
			return 0, true, nil
		}
		return 0, false, fmt.Errorf("record webhook event failure: %w", err)
	}
	return int(row.Attempts), row.Failed, nil
}

// Backlog reports how much inbox work is waiting or stuck.
func (r *EventRepository) Backlog(ctx context.Context) (pending, failed int64, oldest time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	row, err := r.queries.WebhookInboxBacklog(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read inbox backlog: %w", err)
	}
	return row.Pending, row.Failed, time.Duration(row.OldestAgeSeconds * float64(time.Second)), nil
}

func nullableText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	const maxLength = 500
	if len(value) > maxLength {
		value = value[:maxLength]
	}
	return sql.NullString{String: value, Valid: true}
}

var _ app.Repository = (*EventRepository)(nil)
