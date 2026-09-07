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

	app "github.com/rmotti/payments-boilerplate/internal/application/outbox"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/database"
)

// seedMessage stores one webhook event with its outbox message, the way the
// API does, and returns the ids.
func seedMessage(t *testing.T, db *database.Database, suffix string) (eventID, messageID, providerEventID string) {
	t.Helper()

	eventID, _ = domain.NewEventID()
	messageID, _ = domain.NewMessageID()
	providerEventID = "evt_relay_" + suffix + "_" + eventID
	raw := []byte(`{"id":"` + providerEventID + `","object":"event"}`)
	now := time.Now().UTC().Truncate(time.Microsecond)

	event, err := domain.NewEvent(eventID, domain.Stripe, providerEventID,
		"checkout.session.completed", domain.KindCheckoutCompleted, raw, json.RawMessage(raw), now)
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	message, err := domain.NewMessage(messageID, event, "corr-"+suffix, now, now)
	if err != nil {
		t.Fatalf("build message: %v", err)
	}
	if _, err := NewWebhookRepository(db.SQL).StorePending(context.Background(), event, message); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.SQL.ExecContext(context.Background(), "DELETE FROM outbox_events WHERE webhook_event_id = $1", eventID)
		_, _ = db.SQL.ExecContext(context.Background(), "DELETE FROM webhook_events WHERE id = $1", eventID)
	})
	return eventID, messageID, providerEventID
}

func messageStatus(t *testing.T, db *database.Database, messageID string) (status string, attempts int, published bool) {
	t.Helper()
	var publishedAt *time.Time
	if err := db.SQL.QueryRowContext(context.Background(),
		"SELECT status, attempts, published_at FROM outbox_events WHERE id = $1", messageID).
		Scan(&status, &attempts, &publishedAt); err != nil {
		t.Fatalf("read message state: %v", err)
	}
	return status, attempts, publishedAt != nil
}

func TestOutboxRelayLeasesPublishesAndMarks(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	_, messageID, _ := seedMessage(t, db, "ok")
	repository := NewOutboxRepository(db.SQL)

	leases, err := repository.Lease(ctx, "relay-a", 10, time.Minute)
	if err != nil {
		t.Fatalf("Lease() error = %v", err)
	}
	if len(leases) != 1 || leases[0].Message.ID != messageID {
		t.Fatalf("leases = %#v, want the seeded message", leases)
	}

	// While leased, the row is not offered to anyone else.
	others, err := repository.Lease(ctx, "relay-b", 10, time.Minute)
	if err != nil {
		t.Fatalf("second Lease() error = %v", err)
	}
	if len(others) != 0 {
		t.Fatalf("second relay leased %d messages, want none while the lease holds", len(others))
	}

	if err := repository.Published(ctx, "relay-a", messageID); err != nil {
		t.Fatalf("Published() error = %v", err)
	}
	status, attempts, published := messageStatus(t, db, messageID)
	if status != "published" || !published || attempts != 1 {
		t.Fatalf("state = %s/%d/%v, want published with a timestamp", status, attempts, published)
	}
}

// A relay that dies mid-publish must not strand its messages: the lease
// expires and another instance picks them up.
func TestOutboxRelayReclaimsExpiredLease(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	_, messageID, _ := seedMessage(t, db, "expired")
	repository := NewOutboxRepository(db.SQL)

	// A lease that is already over, as a crashed relay would leave behind.
	if _, err := repository.Lease(ctx, "relay-dead", 10, -time.Minute); err != nil {
		t.Fatalf("Lease() error = %v", err)
	}

	leases, err := repository.Lease(ctx, "relay-live", 10, time.Minute)
	if err != nil {
		t.Fatalf("reclaiming Lease() error = %v", err)
	}
	if len(leases) != 1 || leases[0].Message.ID != messageID {
		t.Fatalf("leases = %#v, want the abandoned message reclaimed", leases)
	}
}

// Settling is scoped to the lease holder, so a relay whose lease expired
// cannot overwrite the work of whoever took the message over.
func TestOutboxRelaySettlementRequiresLeaseOwnership(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	_, messageID, _ := seedMessage(t, db, "ownership")
	repository := NewOutboxRepository(db.SQL)

	if _, err := repository.Lease(ctx, "relay-a", 10, time.Minute); err != nil {
		t.Fatalf("Lease() error = %v", err)
	}

	// A stale relay tries to settle a message it no longer holds. It must not
	// only fail to write: it must be told, or the relay would report a
	// publication that nothing recorded.
	err := repository.Published(ctx, "relay-stale", messageID)
	if !errors.Is(err, app.ErrLeaseLost) {
		t.Fatalf("Published() error = %v, want ErrLeaseLost", err)
	}
	if err := repository.Retry(ctx, "relay-stale", messageID, app.ErrNotConfirmed, time.Hour); !errors.Is(err, app.ErrLeaseLost) {
		t.Fatalf("Retry() error = %v, want ErrLeaseLost", err)
	}
	if err := repository.Failed(ctx, "relay-stale", messageID, app.ErrNotConfirmed); !errors.Is(err, app.ErrLeaseLost) {
		t.Fatalf("Failed() error = %v, want ErrLeaseLost", err)
	}

	status, _, published := messageStatus(t, db, messageID)
	if status == "published" || published {
		t.Fatal("a relay without the lease must not be able to mark the message published")
	}
}

// An expired lease must not be settleable either, even by the relay that held
// it: another instance may already have taken the message over.
func TestOutboxRelaySettlementRequiresLiveLease(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	_, messageID, _ := seedMessage(t, db, "stale-lease")
	repository := NewOutboxRepository(db.SQL)

	// A lease that is already over the moment it is taken.
	if _, err := repository.Lease(ctx, "relay-a", 10, -time.Second); err != nil {
		t.Fatalf("Lease() error = %v", err)
	}
	if err := repository.Published(ctx, "relay-a", messageID); !errors.Is(err, app.ErrLeaseLost) {
		t.Fatalf("Published() error = %v, want ErrLeaseLost on an expired lease", err)
	}
}

// The database stamps the deadline, so the relay learns when its lease ends
// without consulting its own clock.
func TestLeaseCarriesDatabaseDeadline(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	seedMessage(t, db, "deadline")

	leases, err := NewOutboxRepository(db.SQL).Lease(ctx, "relay-a", 10, 90*time.Second)
	if err != nil {
		t.Fatalf("Lease() error = %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want one", len(leases))
	}
	if leases[0].Expires.IsZero() {
		t.Fatal("the lease must carry the deadline the database stamped")
	}
	if !leases[0].Expires.After(time.Now().UTC()) {
		t.Fatalf("Expires = %v, want a future deadline", leases[0].Expires)
	}
}

// A transient failure reschedules the message instead of consuming a budget.
func TestOutboxRelayRetryReschedulesMessage(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	_, messageID, _ := seedMessage(t, db, "retry")
	repository := NewOutboxRepository(db.SQL)

	if _, err := repository.Lease(ctx, "relay-a", 10, time.Minute); err != nil {
		t.Fatalf("Lease() error = %v", err)
	}
	if err := repository.Retry(ctx, "relay-a", messageID, app.ErrNotConfirmed, time.Hour); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}

	status, attempts, published := messageStatus(t, db, messageID)
	if status != "pending" || published || attempts != 1 {
		t.Fatalf("state = %s/%d/%v, want it pending with the attempt counted", status, attempts, published)
	}

	// Not due yet: the backoff must actually keep it out of the next batch.
	leases, err := repository.Lease(ctx, "relay-a", 10, time.Minute)
	if err != nil {
		t.Fatalf("Lease() after retry error = %v", err)
	}
	if len(leases) != 0 {
		t.Fatal("a message waiting on its backoff must not be leased again immediately")
	}
}

// Two relays must never publish the same message. The lease, not a held row
// lock, is what guarantees it.
func TestOutboxRelayConcurrentInstancesNeverPublishTwice(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	db := openMigratedTestDatabase(t, databaseURL)

	const messages = 12
	seeded := make(map[string]bool, messages)
	for i := 0; i < messages; i++ {
		_, messageID, _ := seedMessage(t, db, "concurrent")
		seeded[messageID] = true
	}

	const relays = 4
	var mu sync.Mutex
	publishCounts := make(map[string]int)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, relays)
	for i := 0; i < relays; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			owner := fmt.Sprintf("relay-%d", index)
			repository := NewOutboxRepository(db.SQL)
			<-start
			for cycle := 0; cycle < 5; cycle++ {
				leases, err := repository.Lease(context.Background(), owner, 5, time.Minute)
				if err != nil {
					errs <- err
					return
				}
				for _, leased := range leases {
					mu.Lock()
					publishCounts[leased.Message.ID]++
					mu.Unlock()
					// Publishing takes time; the lease must cover it.
					time.Sleep(3 * time.Millisecond)
					if err := repository.Published(context.Background(), owner, leased.Message.ID); err != nil {
						errs <- err
						return
					}
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent relay error = %v", err)
	}

	for messageID := range seeded {
		if publishCounts[messageID] != 1 {
			t.Fatalf("message %s published %d times, want exactly once",
				messageID, publishCounts[messageID])
		}
		status, _, published := messageStatus(t, db, messageID)
		if status != "published" || !published {
			t.Fatalf("message %s state = %s/%v, want published", messageID, status, published)
		}
	}
}

// The relay must only ever see messages, never the provider payload.
func TestOutboxRelayCarriesNoProviderPayload(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	eventID, _, providerEventID := seedMessage(t, db, "claimcheck")

	leases, err := NewOutboxRepository(db.SQL).Lease(ctx, "relay-a", 10, time.Minute)
	if err != nil {
		t.Fatalf("Lease() error = %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want one", len(leases))
	}
	claimed := leases[0].Message

	if claimed.EventID != eventID {
		t.Fatalf("EventID = %q, want the reference to the stored event", claimed.EventID)
	}
	encoded, err := json.Marshal(claimed)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if strings.Contains(string(encoded), providerEventID) {
		t.Fatalf("message must not carry provider data: %s", encoded)
	}
	if claimed.RoutingKey != "payment.webhook.checkout.completed" {
		t.Fatalf("RoutingKey = %q, want the domain routing key", claimed.RoutingKey)
	}
}

// Backlog is what an operator watches; it must count work still to be done.
func TestOutboxBacklogCountsUnpublishedWork(t *testing.T) {
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	ctx := context.Background()
	db := openMigratedTestDatabase(t, databaseURL)
	repository := NewOutboxRepository(db.SQL)

	before, _, err := repository.Backlog(ctx)
	if err != nil {
		t.Fatalf("Backlog() error = %v", err)
	}
	_, messageID, _ := seedMessage(t, db, "backlog")

	after, age, err := repository.Backlog(ctx)
	if err != nil {
		t.Fatalf("Backlog() error = %v", err)
	}
	if after != before+1 {
		t.Fatalf("pending = %d, want %d", after, before+1)
	}
	if age < 0 {
		t.Fatalf("oldest age = %v, want a non-negative duration", age)
	}

	if _, err := repository.Lease(ctx, "relay-a", 10, time.Minute); err != nil {
		t.Fatalf("Lease() error = %v", err)
	}
	if err := repository.Published(ctx, "relay-a", messageID); err != nil {
		t.Fatalf("Published() error = %v", err)
	}
	settled, _, err := repository.Backlog(ctx)
	if err != nil {
		t.Fatalf("Backlog() error = %v", err)
	}
	if settled != before {
		t.Fatalf("pending after publishing = %d, want %d", settled, before)
	}
}
