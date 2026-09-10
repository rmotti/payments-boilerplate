package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	stripeadapter "github.com/rmotti/payments-boilerplate/internal/adapters/payments/stripe"
)

//go:embed fixtures/*.json
var fixtureFiles embed.FS

// Reference identifies the latest payment attempt for a sandbox order.
type Reference struct {
	OrderID         string
	PaymentID       string
	AttemptID       string
	SessionID       string
	PaymentIntentID string
	Amount          int64
	Currency        string
}

// Scenario describes the expected aggregate state after one fixture is
// consumed successfully.
type Scenario struct {
	Name          string `json:"scenario"`
	Fixture       string `json:"-"`
	OrderStatus   string `json:"orderStatus"`
	PaymentStatus string `json:"paymentStatus"`
	AttemptStatus string `json:"attemptStatus"`
	WebhookStatus string `json:"webhookStatus"`
	OutboxStatus  string `json:"outboxStatus"`
}

var scenarios = map[string]Scenario{
	"card-paid": {
		Name: "card-paid", Fixture: "fixtures/card-paid.json",
		OrderStatus: "paid", PaymentStatus: "succeeded", AttemptStatus: "succeeded",
		WebhookStatus: "processed", OutboxStatus: "published",
	},
	"pix-processing": {
		Name: "pix-processing", Fixture: "fixtures/pix-processing.json",
		OrderStatus: "pending", PaymentStatus: "processing", AttemptStatus: "pending",
		WebhookStatus: "processed", OutboxStatus: "published",
	},
	"pix-succeeded": {
		Name: "pix-succeeded", Fixture: "fixtures/pix-succeeded.json",
		OrderStatus: "paid", PaymentStatus: "succeeded", AttemptStatus: "succeeded",
		WebhookStatus: "processed", OutboxStatus: "published",
	},
	"pix-failed": {
		Name: "pix-failed", Fixture: "fixtures/pix-failed.json",
		OrderStatus: "pending", PaymentStatus: "failed", AttemptStatus: "failed",
		WebhookStatus: "processed", OutboxStatus: "published",
	},
	"checkout-expired": {
		Name: "checkout-expired", Fixture: "fixtures/checkout-expired.json",
		OrderStatus: "pending", PaymentStatus: "pending", AttemptStatus: "expired",
		WebhookStatus: "processed", OutboxStatus: "published",
	},
}

type fixtureEnvelope struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	APIVersion string `json:"api_version"`
	Created    int64  `json:"created"`
	Type       string `json:"type"`
	Data       struct {
		Object fixtureSession `json:"object"`
	} `json:"data"`
}

type fixtureSession struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	PaymentStatus     string            `json:"payment_status"`
	Status            string            `json:"status"`
	AmountTotal       int64             `json:"amount_total"`
	Currency          string            `json:"currency"`
	ClientReferenceID string            `json:"client_reference_id"`
	PaymentIntent     *fixtureObject    `json:"payment_intent,omitempty"`
	Metadata          map[string]string `json:"metadata"`
}

type fixtureObject struct {
	ID     string `json:"id"`
	Object string `json:"object"`
}

// ScenarioNames returns the stable names accepted by the emit command.
func ScenarioNames() []string {
	return []string{"card-paid", "pix-processing", "pix-succeeded", "pix-failed", "checkout-expired"}
}

// RenderEvent fills a versioned fixture with references from the local
// checkout. The fixture remains valid JSON throughout; identifiers are never
// interpolated into raw JSON text.
func RenderEvent(name, eventID string, reference Reference, now time.Time) ([]byte, Scenario, string, error) {
	scenario, ok := scenarios[name]
	if !ok {
		return nil, Scenario{}, "", fmt.Errorf("unknown scenario %q; choose %s", name, strings.Join(ScenarioNames(), ", "))
	}
	if strings.TrimSpace(eventID) == "" {
		suffix, err := randomSuffix()
		if err != nil {
			return nil, Scenario{}, "", err
		}
		eventID = "evt_sandbox_" + suffix
	}
	if err := reference.validate(); err != nil {
		return nil, Scenario{}, "", err
	}

	raw, err := fixtureFiles.ReadFile(scenario.Fixture)
	if err != nil {
		return nil, Scenario{}, "", fmt.Errorf("read sandbox fixture: %w", err)
	}
	var event fixtureEnvelope
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, Scenario{}, "", fmt.Errorf("decode sandbox fixture: %w", err)
	}

	event.ID = eventID
	event.APIVersion = stripeadapter.SupportedAPIVersion
	event.Created = now.UTC().Unix()
	event.Data.Object.ID = reference.SessionID
	event.Data.Object.AmountTotal = reference.Amount
	event.Data.Object.Currency = strings.ToLower(reference.Currency)
	event.Data.Object.ClientReferenceID = reference.OrderID
	if reference.PaymentIntentID == "" {
		event.Data.Object.PaymentIntent = nil
	} else if event.Data.Object.PaymentIntent != nil {
		event.Data.Object.PaymentIntent.ID = reference.PaymentIntentID
	}
	event.Data.Object.Metadata = map[string]string{
		"order_id": reference.OrderID, "payment_id": reference.PaymentID,
		"payment_attempt_id": reference.AttemptID,
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return nil, Scenario{}, "", fmt.Errorf("encode sandbox fixture: %w", err)
	}
	return payload, scenario, eventID, nil
}

func (r Reference) validate() error {
	if strings.TrimSpace(r.OrderID) == "" || strings.TrimSpace(r.PaymentID) == "" ||
		strings.TrimSpace(r.AttemptID) == "" || strings.TrimSpace(r.SessionID) == "" {
		return errors.New("sandbox reference requires order, payment, attempt and Checkout Session ids")
	}
	if r.Amount <= 0 || len(r.Currency) != 3 {
		return errors.New("sandbox reference requires a positive amount and three-letter currency")
	}
	return nil
}

// StripeSignature signs the exact body using the same scheme validated by the
// Stripe SDK. It is for local fixtures only and is never persisted.
func StripeSignature(payload []byte, secret string, now time.Time) (string, error) {
	if !strings.HasPrefix(secret, "whsec_") {
		return "", errors.New("webhook secret must begin with whsec_")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := fmt.Fprintf(mac, "%d.%s", now.Unix(), payload); err != nil {
		return "", fmt.Errorf("sign sandbox event: %w", err)
	}
	return fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(mac.Sum(nil))), nil
}
