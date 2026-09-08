package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	dbgen "github.com/rmotti/payments-boilerplate/internal/adapters/postgres/queries"
	app "github.com/rmotti/payments-boilerplate/internal/application/outbox"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
)

// leaseTimeout bounds the claim statement. It is short because the statement
// is short: no network I/O happens while it runs.
const leaseTimeout = 5 * time.Second

// OutboxRepository leases pending messages and records what publishing them
// produced. Each call is its own brief transaction, so no database lock is
// ever held across a call to the broker.
type OutboxRepository struct {
	db      *sql.DB
	queries *dbgen.Queries
}

// NewOutboxRepository binds the repository to the shared pool.
func NewOutboxRepository(db *sql.DB) *OutboxRepository {
	return &OutboxRepository{db: db, queries: dbgen.New(db)}
}

// Lease claims a batch of due messages for owner.
//
// A message is due when it is pending past its backoff, or when it was leased
// and never settled before its deadline, which is how a relay that died
// releases its work without any external coordination.
//
// The deadline is computed by PostgreSQL from the duration given here, never
// by this process. With several instances, the database is the only clock they
// share; a machine running fast would otherwise declare another instance's
// lease expired while it is still publishing.
func (r *OutboxRepository) Lease(
	ctx context.Context,
	owner string,
	batchSize int,
	leaseDuration time.Duration,
) ([]app.Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, leaseTimeout)
	defer cancel()

	rows, err := r.queries.LeaseOutboxBatch(ctx, dbgen.LeaseOutboxBatchParams{
		BatchSize:    int32(batchSize),
		LeaseSeconds: leaseDuration.Seconds(),
		Owner:        sql.NullString{String: owner, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("lease outbox batch: %w", err)
	}

	leases := make([]app.Lease, 0, len(rows))
	for _, row := range rows {
		lease := app.Lease{
			Message:  messageFromRow(row),
			Attempts: int(row.Attempts),
		}
		if row.LockedUntil.Valid {
			lease.Expires = row.LockedUntil.Time.UTC()
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

// Published records a confirmed publication.
//
// The lease is part of the WHERE clause: if it expired and another instance
// took the message over, this update matches nothing rather than overwriting
// the other's work — and says so, instead of reporting a success it did not
// record.
func (r *OutboxRepository) Published(ctx context.Context, owner, messageID string) error {
	ctx, cancel := context.WithTimeout(ctx, app.SettlementReserve)
	defer cancel()

	affected, err := r.queries.MarkOutboxPublished(ctx, dbgen.MarkOutboxPublishedParams{
		ID:    messageID,
		Owner: sql.NullString{String: owner, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}
	return leaseOutcome(affected)
}

// Retry returns a message to the pool, due again after backoff.
func (r *OutboxRepository) Retry(
	ctx context.Context,
	owner, messageID string,
	cause error,
	backoff time.Duration,
) error {
	ctx, cancel := context.WithTimeout(ctx, app.SettlementReserve)
	defer cancel()

	affected, err := r.queries.MarkOutboxRetryable(ctx, dbgen.MarkOutboxRetryableParams{
		ID:             messageID,
		LastError:      errorText(cause),
		BackoffSeconds: backoff.Seconds(),
		Owner:          sql.NullString{String: owner, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("mark outbox retryable: %w", err)
	}
	return leaseOutcome(affected)
}

// Failed stops retrying a message whose error was classified permanent.
func (r *OutboxRepository) Failed(ctx context.Context, owner, messageID string, cause error) error {
	ctx, cancel := context.WithTimeout(ctx, app.SettlementReserve)
	defer cancel()

	affected, err := r.queries.MarkOutboxFailed(ctx, dbgen.MarkOutboxFailedParams{
		ID:        messageID,
		LastError: errorText(cause),
		Owner:     sql.NullString{String: owner, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("mark outbox failed: %w", err)
	}
	return leaseOutcome(affected)
}

// leaseOutcome turns "no row matched" into an explicit lost lease. Reporting
// success here would make the relay claim an outcome nothing recorded.
func leaseOutcome(affected int64) error {
	if affected == 0 {
		return app.ErrLeaseLost
	}
	return nil
}

// Backlog reports how much work is waiting and how old the oldest item is.
func (r *OutboxRepository) Backlog(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	row, err := r.queries.OutboxBacklog(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("read outbox backlog: %w", err)
	}
	return row.Pending, time.Duration(row.OldestAgeSeconds * float64(time.Second)), nil
}

func messageFromRow(row dbgen.LeaseOutboxBatchRow) domain.Message {
	return domain.Message{
		ID:            row.ID,
		EventID:       row.WebhookEventID,
		Kind:          domain.Kind(row.EventType),
		SchemaVersion: int(row.SchemaVersion),
		RoutingKey:    row.RoutingKey,
		CorrelationID: row.CorrelationID,
		OccurredAt:    row.OccurredAt.UTC(),
		Status:        domain.MessageStatusPending,
	}
}

// errorText is the single point both the outbox relay and the inbox
// RecordFailure path use to turn a cause into what last_error stores. It is
// the superset of what an integrator can read back from the operational API,
// so a cause is sanitized before it is truncated: truncating first would
// still leave a credential intact if it falls inside the kept prefix, which
// is exactly what a driver or broker error beginning with a DSN does.
func errorText(err error) sql.NullString {
	if err == nil {
		return sql.NullString{}
	}
	text := errsanitize.Sanitize(err.Error())
	return sql.NullString{String: text, Valid: true}
}

var _ app.Repository = (*OutboxRepository)(nil)
