package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	dbgen "github.com/rmotti/payments-boilerplate/internal/adapters/postgres/queries"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

// WebhookRepository persists the inbox and the outbox.
//
// It uses sqlc over database/sql rather than GORM: the shape of these
// statements is part of the guarantee, and ADR 0007 reserves explicit SQL for
// exactly that. Both tables are new, so no GORM transaction is mixed in.
type WebhookRepository struct {
	db      *sql.DB
	queries *dbgen.Queries
}

// NewWebhookRepository binds the repository to the shared pool.
func NewWebhookRepository(db *sql.DB) *WebhookRepository {
	return &WebhookRepository{db: db, queries: dbgen.New(db)}
}

// StoreIgnored persists an event that produces no outbox message. It reports
// whether the row was new; a redelivery inserts nothing.
func (r *WebhookRepository) StoreIgnored(ctx context.Context, event domain.Event) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	return insertedEvent(r.queries.InsertWebhookEvent(ctx, eventParams(event)))
}

// StorePending persists the event and its message atomically. Either both rows
// exist or neither does, which is what removes the window where an accepted
// event could never be published.
func (r *WebhookRepository) StorePending(
	ctx context.Context,
	event domain.Event,
	message domain.Message,
) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin webhook transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	queries := r.queries.WithTx(tx)
	inserted, err := insertedEvent(queries.InsertWebhookEvent(ctx, eventParams(event)))
	if err != nil {
		return false, err
	}
	if !inserted {
		// A redelivery: the first arrival already committed both rows.
		return false, nil
	}

	if err := queries.InsertOutboxEvent(ctx, dbgen.InsertOutboxEventParams{
		ID:             message.ID,
		WebhookEventID: message.EventID,
		EventType:      string(message.Kind),
		SchemaVersion:  int32(message.SchemaVersion),
		RoutingKey:     message.RoutingKey,
		CorrelationID:  message.CorrelationID,
		OccurredAt:     message.OccurredAt,
		Status:         string(message.Status),
		CreatedAt:      message.CreatedAt,
	}); err != nil {
		return false, fmt.Errorf("insert outbox event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit webhook transaction: %w", err)
	}
	return true, nil
}

func eventParams(event domain.Event) dbgen.InsertWebhookEventParams {
	return dbgen.InsertWebhookEventParams{
		ID:              event.ID,
		Provider:        string(event.Provider),
		ProviderEventID: event.ProviderEventID,
		EventType:       event.Type,
		RawPayload:      event.RawPayload,
		Payload:         event.Payload,
		Status:          string(event.Status),
		ReceivedAt:      event.ReceivedAt,
	}
}

// insertedEvent turns the ON CONFLICT DO NOTHING result into a boolean: no row
// returned means the provider event id was already present.
func insertedEvent(_ string, err error) (bool, error) {
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("insert webhook event: %w", err)
	}
	return true, nil
}
