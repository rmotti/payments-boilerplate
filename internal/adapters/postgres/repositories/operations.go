package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	dbgen "github.com/rmotti/payments-boilerplate/internal/adapters/postgres/queries"
	app "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
)

// List returns a bounded operational view without exposing stored provider
// payloads, which can contain customer data.
func (r *WebhookRepository) List(
	ctx context.Context,
	status domain.Status,
	limit int,
) ([]app.EventInspection, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	rows, err := r.queries.ListWebhookEvents(ctx, dbgen.ListWebhookEventsParams{
		Status: string(status), ResultLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list webhook events: %w", err)
	}
	result := make([]app.EventInspection, 0, len(rows))
	for _, row := range rows {
		result = append(result, inspectionFromRow(row))
	}
	return result, nil
}

// Reprocess atomically makes failed work eligible for the outbox relay again.
// It reuses the existing message, so concurrent operator requests cannot make
// multiple durable messages for one provider event.
func (r *WebhookRepository) Reprocess(ctx context.Context, eventID string) (app.EventInspection, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return app.EventInspection{}, fmt.Errorf("begin webhook replay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	queries := r.queries.WithTx(tx)
	locked, err := queries.LockWebhookEventForReplay(ctx, eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, lookupErr := queries.GetWebhookEvent(ctx, eventID)
			if lookupErr == nil {
				return app.EventInspection{}, app.ErrEventNotReplayable
			}
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return app.EventInspection{}, app.ErrEventNotFound
			}
			return app.EventInspection{}, fmt.Errorf("check webhook event for replay: %w", lookupErr)
		}
		return app.EventInspection{}, fmt.Errorf("lock webhook event for replay: %w", err)
	}
	if (locked.EventStatus != string(domain.StatusPending) && locked.EventStatus != string(domain.StatusFailed)) ||
		(locked.EventStatus != string(domain.StatusFailed) && locked.OutboxStatus != string(domain.MessageStatusFailed)) {
		return app.EventInspection{}, app.ErrEventNotReplayable
	}

	eventRows, err := queries.ResetWebhookEventForReplay(ctx, eventID)
	if err != nil {
		return app.EventInspection{}, fmt.Errorf("reset webhook event for replay: %w", err)
	}
	outboxRows, err := queries.ResetOutboxForReplay(ctx, eventID)
	if err != nil {
		return app.EventInspection{}, fmt.Errorf("reset outbox event for replay: %w", err)
	}
	if eventRows != 1 || outboxRows != 1 {
		return app.EventInspection{}, fmt.Errorf("replay invariant: reset %d inbox and %d outbox rows", eventRows, outboxRows)
	}
	row, err := queries.GetWebhookEvent(ctx, eventID)
	if err != nil {
		return app.EventInspection{}, fmt.Errorf("read replayed webhook event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return app.EventInspection{}, fmt.Errorf("commit webhook replay transaction: %w", err)
	}
	return inspectionFromGetRow(row), nil
}

func inspectionFromRow(row dbgen.ListWebhookEventsRow) app.EventInspection {
	return inspectionFromValues(
		row.ID, row.Provider, row.ProviderEventID, row.EventType, row.Status,
		row.Attempts, row.ReceivedAt, row.ProcessedAt, row.LastError, row.UpdatedAt,
		row.ReplayCount, row.LastReplayedAt, row.OutboxID, row.OutboxStatus,
		row.OutboxAttempts, row.OutboxPublishedAt, row.OutboxLastError, row.OutboxNextAttemptAt,
	)
}

func inspectionFromGetRow(row dbgen.GetWebhookEventRow) app.EventInspection {
	return inspectionFromValues(
		row.ID, row.Provider, row.ProviderEventID, row.EventType, row.Status,
		row.Attempts, row.ReceivedAt, row.ProcessedAt, row.LastError, row.UpdatedAt,
		row.ReplayCount, row.LastReplayedAt, row.OutboxID, row.OutboxStatus,
		row.OutboxAttempts, row.OutboxPublishedAt, row.OutboxLastError, row.OutboxNextAttemptAt,
	)
}

func inspectionFromValues(
	id, provider, providerEventID, eventType, status string,
	attempts int32,
	receivedAt time.Time,
	processedAt sql.NullTime,
	lastError sql.NullString,
	updatedAt time.Time,
	replayCount int32,
	lastReplayedAt sql.NullTime,
	outboxID, outboxStatus sql.NullString,
	outboxAttempts sql.NullInt32,
	outboxPublishedAt sql.NullTime,
	outboxLastError sql.NullString,
	outboxNextAttemptAt sql.NullTime,
) app.EventInspection {
	result := app.EventInspection{
		ID: id, Provider: domain.Provider(provider), ProviderEventID: providerEventID,
		EventType: eventType, Status: domain.Status(status), Attempts: int(attempts),
		ReceivedAt: receivedAt.UTC(), ProcessedAt: operationNullableTime(processedAt),
		LastError: operationNullableString(lastError), UpdatedAt: updatedAt.UTC(),
		ReplayCount: int(replayCount), LastReplayedAt: operationNullableTime(lastReplayedAt),
	}
	if outboxID.Valid {
		result.Outbox = &app.OutboxInspection{
			ID: outboxID.String, Status: domain.MessageStatus(outboxStatus.String),
			Attempts: int(outboxAttempts.Int32), PublishedAt: operationNullableTime(outboxPublishedAt),
			LastError: operationNullableString(outboxLastError), NextAttemptAt: outboxNextAttemptAt.Time.UTC(),
		}
	}
	return result
}

func operationNullableString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	// Sanitize again at the read boundary so a database created before E8b, or
	// a value written manually by an operator, cannot expose a legacy secret
	// through the operational API.
	return errsanitize.Sanitize(value.String)
}

func operationNullableTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

var _ app.InspectionRepository = (*WebhookRepository)(nil)
