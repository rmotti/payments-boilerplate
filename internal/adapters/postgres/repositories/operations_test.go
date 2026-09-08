package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	app "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
)

func TestWebhookRepositoryInspectsAndSafelyReprocessesFailure(t *testing.T) {
	db := openConsumerTestDatabase(t)
	repository := NewWebhookRepository(db.SQL)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	eventID, _ := domain.NewEventID()
	messageID, _ := domain.NewMessageID()
	const legacySentinel = "legacy-last-error-secret"
	providerEventID := "evt_replay_provider_" + eventID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)
	event, err := domain.NewEvent(eventID, domain.Stripe, providerEventID,
		"checkout.session.completed", domain.KindCheckoutCompleted, raw, json.RawMessage(raw), now)
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	message, err := domain.NewMessage(messageID, event, "corr-replay", now, now)
	if err != nil {
		t.Fatalf("build message: %v", err)
	}
	if stored, err := repository.StorePending(ctx, event, message); err != nil || !stored {
		t.Fatalf("StorePending() = %t, %v", stored, err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(), "DELETE FROM outbox_events WHERE webhook_event_id = $1", eventID)
		_, _ = db.SQL.ExecContext(context.Background(), "DELETE FROM webhook_events WHERE id = $1", eventID)
	})

	if _, err := db.SQL.ExecContext(ctx, `UPDATE webhook_events
			SET status = 'failed', attempts = 10, last_error = $2
			WHERE id = $1`, eventID, "postgres://payments:"+legacySentinel+"@db.internal/payments"); err != nil {
		t.Fatalf("fail inbox event: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx, `UPDATE outbox_events
			SET status = 'published', attempts = 2, published_at = now(), last_error = $2
			WHERE webhook_event_id = $1`, eventID, "amqp://relay:"+legacySentinel+"@rabbitmq.internal/"); err != nil {
		t.Fatalf("publish outbox event: %v", err)
	}

	failed, err := repository.List(ctx, domain.StatusFailed, 10)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(failed) != 1 || failed[0].ID != eventID || failed[0].Outbox == nil {
		t.Fatalf("failed events = %#v, want seeded event with outbox metadata", failed)
	}
	for name, value := range map[string]string{
		"inbox":  failed[0].LastError,
		"outbox": failed[0].Outbox.LastError,
	} {
		if strings.Contains(value, legacySentinel) {
			t.Fatalf("%s last_error = %q, exposed a legacy credential", name, value)
		}
		if !strings.Contains(value, errsanitize.Redacted) {
			t.Fatalf("%s last_error = %q, want %q", name, value, errsanitize.Redacted)
		}
	}

	replayed, err := repository.Reprocess(ctx, eventID)
	if err != nil {
		t.Fatalf("Reprocess() error = %v", err)
	}
	if replayed.Status != domain.StatusPending || replayed.Attempts != 0 ||
		replayed.LastError != "" || replayed.ReplayCount != 1 || replayed.LastReplayedAt == nil {
		t.Fatalf("replayed inbox = %#v, want a fresh pending attempt with audit count", replayed)
	}
	if replayed.Outbox == nil || replayed.Outbox.Status != domain.MessageStatusPending ||
		replayed.Outbox.Attempts != 0 || replayed.Outbox.LastError != "" || replayed.Outbox.PublishedAt != nil {
		t.Fatalf("replayed outbox = %#v, want due pending original message", replayed.Outbox)
	}
	if replayed.Outbox.ID != messageID {
		t.Fatalf("message id = %q, want original %q", replayed.Outbox.ID, messageID)
	}

	if _, err := repository.Reprocess(ctx, eventID); !errors.Is(err, app.ErrEventNotReplayable) {
		t.Fatalf("second Reprocess() error = %v, want %v", err, app.ErrEventNotReplayable)
	}

	// A permanent relay failure happens before the consumer runs: the inbox is
	// still pending while only the outbox is failed. It is replayable too.
	if _, err := db.SQL.ExecContext(ctx, `UPDATE outbox_events
		SET status = 'failed', attempts = 4, last_error = 'encoding failed'
		WHERE webhook_event_id = $1`, eventID); err != nil {
		t.Fatalf("fail outbox event: %v", err)
	}
	publishReplay, err := repository.Reprocess(ctx, eventID)
	if err != nil {
		t.Fatalf("Reprocess() outbox failure error = %v", err)
	}
	if publishReplay.Status != domain.StatusPending || publishReplay.ReplayCount != 2 ||
		publishReplay.Outbox == nil || publishReplay.Outbox.Status != domain.MessageStatusPending {
		t.Fatalf("publication replay = %#v, want pending inbox/outbox with second audit count", publishReplay)
	}
}
