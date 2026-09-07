package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	stripewebhook "github.com/stripe/stripe-go/v86/webhook"
)

const testSecret = "whsec_test_secret"

// signature builds a real Stripe-Signature header for the exact bytes given.
func signature(t *testing.T, payload []byte, secret string, at time.Time) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := fmt.Fprintf(mac, "%d.%s", at.Unix(), payload); err != nil {
		t.Fatalf("sign payload: %v", err)
	}
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// eventPayload mirrors the shape Stripe actually delivers. The top level
// "object":"event" matters: the SDK uses it to tell a full event from a thin
// event notification and refuses to parse the wrong one.
func eventPayload(id, eventType string, created time.Time) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"object":"event","type":%q,"created":%d,`+
			`"data":{"object":{"id":"cs_test_1","object":"checkout.session"}}}`,
		id, eventType, created.Unix()))
}

func TestVerifyAcceptsSignedEvent(t *testing.T) {
	t.Parallel()

	now := time.Now()
	payload := eventPayload("evt_1", "checkout.session.completed", now)
	verifier := NewWebhook(testSecret)

	event, err := verifier.Verify(payload, signature(t, payload, testSecret, now))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if event.ID != "evt_1" {
		t.Fatalf("ID = %q, want evt_1", event.ID)
	}
	if event.Type != "checkout.session.completed" {
		t.Fatalf("Type = %q, want the provider event name", event.Type)
	}
	if event.Kind != domain.KindCheckoutCompleted {
		t.Fatalf("Kind = %q, want the neutral checkout kind", event.Kind)
	}
	if !event.OccurredAt.Equal(now.Truncate(time.Second).UTC()) {
		t.Fatalf("OccurredAt = %v, want the event creation instant", event.OccurredAt)
	}
	if string(event.Payload) != string(payload) {
		t.Fatal("Payload should preserve the signed bytes")
	}
}

func TestVerifyMapsUnhandledTypeToUnknown(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// A type the application does not act on must still verify: it is recorded
	// and ignored, never rejected as if it were forged.
	payload := eventPayload("evt_2", "customer.created", now)
	verifier := NewWebhook(testSecret)

	event, err := verifier.Verify(payload, signature(t, payload, testSecret, now))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if event.Kind != domain.KindUnknown {
		t.Fatalf("Kind = %q, want KindUnknown", event.Kind)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	t.Parallel()

	now := time.Now()
	payload := eventPayload("evt_3", "checkout.session.completed", now)
	header := signature(t, payload, testSecret, now)

	// One byte changed after signing: the recomputed digest must not match.
	tampered := append([]byte(nil), payload...)
	tampered[len(tampered)-2] = '0'

	if _, err := NewWebhook(testSecret).Verify(tampered, header); err == nil {
		t.Fatal("Verify() error = nil, want rejection of tampered bytes")
	}
}

func TestVerifyRejectsForeignSecret(t *testing.T) {
	t.Parallel()

	now := time.Now()
	payload := eventPayload("evt_4", "checkout.session.completed", now)
	header := signature(t, payload, "whsec_someone_else", now)

	if _, err := NewWebhook(testSecret).Verify(payload, header); err == nil {
		t.Fatal("Verify() error = nil, want rejection of a foreign secret")
	}
}

func TestVerifyRejectsMissingSignature(t *testing.T) {
	t.Parallel()

	now := time.Now()
	payload := eventPayload("evt_5", "checkout.session.completed", now)

	_, err := NewWebhook(testSecret).Verify(payload, "")
	if err == nil {
		t.Fatal("Verify() error = nil, want rejection of an unsigned request")
	}
	if !errors.Is(err, stripewebhook.ErrNotSigned) {
		t.Fatalf("Verify() error = %v, want ErrNotSigned", err)
	}
}

func TestVerifyRejectsExpiredTimestamp(t *testing.T) {
	t.Parallel()

	// Replay protection: a capture from outside the tolerance window is
	// refused even though its signature is arithmetically correct.
	old := time.Now().Add(-2 * stripewebhook.DefaultTolerance)
	payload := eventPayload("evt_6", "checkout.session.completed", old)

	_, err := NewWebhook(testSecret).Verify(payload, signature(t, payload, testSecret, old))
	if err == nil {
		t.Fatal("Verify() error = nil, want rejection of an expired signature")
	}
	if !errors.Is(err, stripewebhook.ErrTooOld) {
		t.Fatalf("Verify() error = %v, want ErrTooOld", err)
	}
}
