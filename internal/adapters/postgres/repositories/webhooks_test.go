package repositories

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/database"
	"go.uber.org/zap"
)

func TestWebhookRepositoryIntegration(t *testing.T) {
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

	repository := NewWebhookRepository(db.SQL)
	now := time.Now().UTC().Truncate(time.Microsecond)

	eventID, _ := domain.NewEventID()
	messageID, _ := domain.NewMessageID()
	providerEventID := "evt_provider_" + eventID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM outbox_events WHERE webhook_event_id IN (SELECT id FROM webhook_events WHERE provider_event_id = $1)", providerEventID)
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM webhook_events WHERE provider_event_id = $1", providerEventID)
	})

	event, err := domain.NewEvent(eventID, domain.Stripe, providerEventID,
		"checkout.session.completed", domain.KindCheckoutCompleted, raw, json.RawMessage(raw), now)
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	message, err := domain.NewMessage(messageID, event, "corr-integration", now, now)
	if err != nil {
		t.Fatalf("build message: %v", err)
	}

	stored, err := repository.StorePending(ctx, event, message)
	if err != nil {
		t.Fatalf("StorePending() error = %v", err)
	}
	if !stored {
		t.Fatal("StorePending() stored = false, want the first delivery to be new")
	}

	// Both rows must exist: they are written in one transaction precisely so
	// an accepted event can never end up without its message.
	assertCount(t, db, "SELECT count(*) FROM webhook_events WHERE provider_event_id = $1", providerEventID, 1)
	assertCount(t, db, "SELECT count(*) FROM outbox_events WHERE webhook_event_id = $1", eventID, 1)

	// A redelivery of the same provider event must change nothing. The unique
	// index is what enforces it, not application code.
	replayEventID, _ := domain.NewEventID()
	replayMessageID, _ := domain.NewMessageID()
	replayEvent, err := domain.NewEvent(replayEventID, domain.Stripe, providerEventID,
		"checkout.session.completed", domain.KindCheckoutCompleted, raw, json.RawMessage(raw), now)
	if err != nil {
		t.Fatalf("build replay event: %v", err)
	}
	replayMessage, err := domain.NewMessage(replayMessageID, replayEvent, "corr-replay", now, now)
	if err != nil {
		t.Fatalf("build replay message: %v", err)
	}

	stored, err = repository.StorePending(ctx, replayEvent, replayMessage)
	if err != nil {
		t.Fatalf("StorePending() on redelivery error = %v", err)
	}
	if stored {
		t.Fatal("StorePending() stored = true on redelivery, want the duplicate to be rejected")
	}

	assertCount(t, db, "SELECT count(*) FROM webhook_events WHERE provider_event_id = $1", providerEventID, 1)
	// The rollback matters as much as the insert: a duplicate must not leave a
	// second message behind for the relay to publish.
	assertCount(t, db, "SELECT count(*) FROM outbox_events WHERE id = $1", replayMessageID, 0)

	var status string
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT status FROM webhook_events WHERE provider_event_id = $1", providerEventID).Scan(&status); err != nil {
		t.Fatalf("read stored status: %v", err)
	}
	if status != string(domain.StatusPending) {
		t.Fatalf("status = %q, want pending", status)
	}
}

func TestWebhookRepositoryStoresIgnoredWithoutMessage(t *testing.T) {
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
	t.Cleanup(func() { _ = db.Close() })

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

	repository := NewWebhookRepository(db.SQL)
	now := time.Now().UTC().Truncate(time.Microsecond)

	eventID, _ := domain.NewEventID()
	providerEventID := "evt_ignored_" + eventID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM webhook_events WHERE provider_event_id = $1", providerEventID)
	})

	event, err := domain.NewEvent(eventID, domain.Stripe, providerEventID,
		"customer.created", domain.KindUnknown, raw, json.RawMessage(raw), now)
	if err != nil {
		t.Fatalf("build event: %v", err)
	}

	stored, err := repository.StoreIgnored(ctx, event)
	if err != nil {
		t.Fatalf("StoreIgnored() error = %v", err)
	}
	if !stored {
		t.Fatal("StoreIgnored() stored = false, want the event to be recorded")
	}

	// Recorded for audit, but nothing queued: the consumer must never see it.
	assertCount(t, db, "SELECT count(*) FROM outbox_events WHERE webhook_event_id = $1", eventID, 0)

	var status string
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT status FROM webhook_events WHERE id = $1", eventID).Scan(&status); err != nil {
		t.Fatalf("read stored status: %v", err)
	}
	if status != string(domain.StatusSkipped) {
		t.Fatalf("status = %q, want skipped", status)
	}
}

func assertCount(t *testing.T, db *database.Database, query, argument string, want int) {
	t.Helper()
	var count int
	if err := db.SQL.QueryRowContext(context.Background(), query, argument).Scan(&count); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if count != want {
		t.Fatalf("count for %q = %d, want %d", query, count, want)
	}
}

// The whole point of writing both rows in one transaction is that a failure on
// the second one must undo the first. Without this test, a regression that
// split the write into two transactions would still pass the happy path and
// leave accepted events that no relay can ever publish.
func TestWebhookRepositoryRollsBackInboxWhenOutboxFails(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	repository := NewWebhookRepository(db.SQL)
	now := time.Now().UTC().Truncate(time.Microsecond)

	eventID, _ := domain.NewEventID()
	providerEventID := "evt_rollback_" + eventID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM webhook_events WHERE provider_event_id = $1", providerEventID)
	})

	event, err := domain.NewEvent(eventID, domain.Stripe, providerEventID,
		"checkout.session.completed", domain.KindCheckoutCompleted, raw, json.RawMessage(raw), now)
	if err != nil {
		t.Fatalf("build event: %v", err)
	}

	// A message that the schema must reject: schema_version has a positive
	// check constraint, so the second insert fails after the first succeeded.
	message, err := domain.NewMessage("msg_rollback_"+eventID, event, "corr-rollback", now, now)
	if err != nil {
		t.Fatalf("build message: %v", err)
	}
	message.SchemaVersion = 0

	stored, err := repository.StorePending(ctx, event, message)
	if err == nil {
		t.Fatal("StorePending() error = nil, want the outbox insert to fail")
	}
	if stored {
		t.Fatal("StorePending() stored = true, want failure")
	}

	// Neither row may survive: the event was never durably accepted.
	assertCount(t, db, "SELECT count(*) FROM webhook_events WHERE provider_event_id = $1", providerEventID, 0)
	assertCount(t, db, "SELECT count(*) FROM outbox_events WHERE webhook_event_id = $1", eventID, 0)
}

// Stripe can deliver the same event twice at once, and two API replicas can
// receive them in parallel. Deduplication has to hold under that race, and it
// does because it lives in the unique index rather than in application code.
func TestWebhookRepositoryDeduplicatesConcurrentDeliveries(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	repository := NewWebhookRepository(db.SQL)
	now := time.Now().UTC().Truncate(time.Microsecond)

	sharedEventID, _ := domain.NewEventID()
	providerEventID := "evt_concurrent_" + sharedEventID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM outbox_events WHERE webhook_event_id IN (SELECT id FROM webhook_events WHERE provider_event_id = $1)", providerEventID)
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM webhook_events WHERE provider_event_id = $1", providerEventID)
	})

	const deliveries = 8
	results := make(chan bool, deliveries)
	errs := make(chan error, deliveries)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each delivery proposes its own local ids, exactly as separate
			// requests would; only the provider event id is shared.
			eventID, _ := domain.NewEventID()
			messageID, _ := domain.NewMessageID()
			event, err := domain.NewEvent(eventID, domain.Stripe, providerEventID,
				"checkout.session.completed", domain.KindCheckoutCompleted, raw, json.RawMessage(raw), now)
			if err != nil {
				errs <- err
				return
			}
			message, err := domain.NewMessage(messageID, event, "corr-concurrent", now, now)
			if err != nil {
				errs <- err
				return
			}
			<-start
			stored, err := repository.StorePending(ctx, event, message)
			if err != nil {
				errs <- err
				return
			}
			results <- stored
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("StorePending() concurrent error = %v", err)
	}

	accepted := 0
	total := 0
	for stored := range results {
		total++
		if stored {
			accepted++
		}
	}
	if total != deliveries {
		t.Fatalf("completed deliveries = %d, want %d", total, deliveries)
	}
	if accepted != 1 {
		t.Fatalf("accepted = %d, want exactly one delivery to win the race", accepted)
	}

	// One event, one message: the losers must leave nothing behind.
	assertCount(t, db, "SELECT count(*) FROM webhook_events WHERE provider_event_id = $1", providerEventID, 1)
	assertCount(t, db,
		"SELECT count(*) FROM outbox_events WHERE webhook_event_id IN (SELECT id FROM webhook_events WHERE provider_event_id = $1)",
		providerEventID, 1)
}

// The concurrency test above passes even against a check-then-insert
// implementation, because under READ COMMITTED the loser's insert simply blocks
// on the unique index until the winner commits. That makes it a weak guard on
// its own: it proves the outcome, not where the guarantee lives.
//
// This test pins the guarantee to the schema. If the unique index were ever
// dropped or narrowed, deduplication would silently move into application code
// and start losing races that the database currently wins for us.
func TestWebhookInboxUniquenessIsEnforcedBySchema(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	now := time.Now().UTC().Truncate(time.Microsecond)

	firstID, _ := domain.NewEventID()
	secondID, _ := domain.NewEventID()
	providerEventID := "evt_unique_" + firstID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM webhook_events WHERE provider_event_id = $1", providerEventID)
	})

	insert := `INSERT INTO webhook_events
		(id, provider, provider_event_id, event_type, raw_payload, payload, status, received_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`

	if _, err := db.SQL.ExecContext(ctx, insert, firstID, string(domain.Stripe), providerEventID,
		"checkout.session.completed", raw, raw, string(domain.StatusPending), now); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// A second row for the same provider event, bypassing the repository
	// entirely: the database itself must refuse it.
	_, err := db.SQL.ExecContext(ctx, insert, secondID, string(domain.Stripe), providerEventID,
		"checkout.session.completed", raw, raw, string(domain.StatusPending), now)
	if err == nil {
		t.Fatal("second insert succeeded, want the unique index to reject the duplicate")
	}
	if !strings.Contains(err.Error(), "webhook_events_provider_event_id_key") {
		t.Fatalf("rejection came from %v, want the provider event id unique index", err)
	}
}

// The same argument for the outbox: one message per received event must be a
// schema guarantee, so a relay can never find two messages for one event.
func TestWebhookOutboxOneMessagePerEventIsEnforcedBySchema(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	repository := NewWebhookRepository(db.SQL)
	now := time.Now().UTC().Truncate(time.Microsecond)

	eventID, _ := domain.NewEventID()
	messageID, _ := domain.NewMessageID()
	providerEventID := "evt_onemsg_" + eventID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM outbox_events WHERE webhook_event_id = $1", eventID)
		_, _ = db.SQL.ExecContext(context.Background(),
			"DELETE FROM webhook_events WHERE provider_event_id = $1", providerEventID)
	})

	event, err := domain.NewEvent(eventID, domain.Stripe, providerEventID,
		"checkout.session.completed", domain.KindCheckoutCompleted, raw, json.RawMessage(raw), now)
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	message, err := domain.NewMessage(messageID, event, "corr-onemsg", now, now)
	if err != nil {
		t.Fatalf("build message: %v", err)
	}
	if _, err := repository.StorePending(ctx, event, message); err != nil {
		t.Fatalf("StorePending() error = %v", err)
	}

	otherMessageID, _ := domain.NewMessageID()
	_, err = db.SQL.ExecContext(ctx, `INSERT INTO outbox_events
		(id, webhook_event_id, event_type, schema_version, routing_key, correlation_id, occurred_at, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		otherMessageID, eventID, string(domain.KindCheckoutCompleted), domain.SchemaVersion,
		"payment.webhook.checkout.completed", "corr-second", now, string(domain.MessageStatusPending), now)
	if err == nil {
		t.Fatal("second message succeeded, want one message per received event")
	}
	if !strings.Contains(err.Error(), "outbox_events_webhook_event_id_key") {
		t.Fatalf("rejection came from %v, want the one-message-per-event index", err)
	}
}

// openMigratedTestDatabase opens the pool and brings the schema up to date.
func openMigratedTestDatabase(t *testing.T, databaseURL string) *database.Database {
	t.Helper()

	db, err := database.Open(context.Background(), config.Config{
		DatabaseURL: databaseURL, DatabaseMaxOpenConnections: 16,
		DatabaseMaxIdleConnections: 4, DatabaseConnectionMaxLifetime: time.Minute,
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
	return db
}
