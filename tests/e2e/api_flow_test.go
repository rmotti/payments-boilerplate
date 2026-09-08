//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	stripeadapter "github.com/rmotti/payments-boilerplate/internal/adapters/payments/stripe"
)

type orderResponse struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type checkoutResponse struct {
	CheckoutURL string    `json:"checkoutUrl"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

func TestAPIOrderCheckoutAndAuthenticationFlow(t *testing.T) {
	h := newHarness(t, false)

	response, body := h.request(http.MethodPost, "/v1/orders", "", map[string]any{
		"productId": "product_demo", "quantity": 1,
	}, map[string]string{"Idempotency-Key": "order-auth-missing"})
	requireStatus(t, response, body, http.StatusUnauthorized)
	response, body = h.request(http.MethodPost, "/v1/orders", "invalid-key", map[string]any{
		"productId": "product_demo", "quantity": 1,
	}, map[string]string{"Idempotency-Key": "order-auth-invalid"})
	requireStatus(t, response, body, http.StatusUnauthorized)
	if got := scanCount(t, h.db, "SELECT count(*) FROM orders"); got != 0 {
		t.Fatalf("orders after rejected authentication = %d, want 0", got)
	}

	// amount and currency are deliberately sent by the caller. They are not in
	// the application input and must never override the catalog price.
	created := h.createOrder(apiKeyA, "order-price", map[string]any{
		"productId": "product_demo", "quantity": 2, "amount": 1, "currency": "USD",
	})
	if created.Amount != 20000 || created.Currency != "BRL" || created.Status != "pending" {
		t.Fatalf("server-priced order = %#v, want pending 20000 BRL", created)
	}

	// The second active integration key can read an order created with the
	// first. This records the deliberate single-integrator scope.
	response, body = h.request(http.MethodGet, "/v1/orders/"+created.ID, apiKeyB, nil, nil)
	requireStatus(t, response, body, http.StatusOK)

	replayed := h.createOrder(apiKeyA, "order-price", map[string]any{
		"productId": "product_demo", "quantity": 2,
	})
	if replayed.ID != created.ID || scanCount(t, h.db, "SELECT count(*) FROM orders") != 1 {
		t.Fatalf("identical replay = %#v, original=%#v", replayed, created)
	}

	response, body = h.request(http.MethodPost, "/v1/orders", apiKeyA, map[string]any{
		"productId": "product_demo", "quantity": 3,
	}, map[string]string{"Idempotency-Key": "order-price"})
	requireStatus(t, response, body, http.StatusConflict)
	response, body = h.request(http.MethodPost, "/v1/orders", apiKeyA, map[string]any{
		"productId": "does_not_exist", "quantity": 1,
	}, map[string]string{"Idempotency-Key": "order-product-missing"})
	requireStatus(t, response, body, http.StatusNotFound)

	checkout := h.createCheckout(created.ID, "checkout-one")
	replay := h.createCheckout(created.ID, "checkout-one")
	if replay != checkout {
		t.Fatalf("checkout replay = %#v, want %#v", replay, checkout)
	}
	requests, sessions := h.provider.snapshot()
	if len(requests) != 1 || len(sessions) != 1 {
		t.Fatalf("provider calls = %d, sessions = %d, want one of each", len(requests), len(sessions))
	}
	request := requests[0]
	if request.OrderID != created.ID || request.UnitAmount != 10000 || request.Quantity != 2 ||
		request.Currency != "BRL" || request.IdempotencyKey != "checkout-one" {
		t.Fatalf("provider request contains untrusted or wrong values: %#v", request)
	}

	response, body = h.request(http.MethodPost, "/v1/orders/"+created.ID+"/checkout", apiKeyA, nil, nil)
	requireStatus(t, response, body, http.StatusBadRequest)
	response, body = h.request(http.MethodPost, "/v1/orders/"+created.ID+"/checkout", apiKeyA, nil,
		map[string]string{"Idempotency-Key": "checkout-two"})
	requireStatus(t, response, body, http.StatusConflict)
	secondOrder := h.createOrder(apiKeyA, "order-second", map[string]any{
		"productId": "product_demo", "quantity": 1,
	})
	response, body = h.request(http.MethodPost, "/v1/orders/"+secondOrder.ID+"/checkout", apiKeyA, nil,
		map[string]string{"Idempotency-Key": "checkout-one"})
	requireStatus(t, response, body, http.StatusConflict)
}

func TestWebhookReceptionIsSignedAtomicAndDeduplicated(t *testing.T) {
	h := newHarness(t, false)

	invalid := []byte(`{"id":"evt_e2e_invalid","object":"event","type":"checkout.session.completed","data":{"object":{}}}`)
	response, body := h.postWebhook(invalid, false)
	requireStatus(t, response, body, http.StatusBadRequest)
	if got := scanCount(t, h.db, "SELECT count(*) FROM webhook_events"); got != 0 {
		t.Fatalf("invalid signature persisted %d inbox rows", got)
	}

	payload := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_received", Type: "checkout.session.completed", SessionID: "cs_received",
		OrderID: "ord_00000000000000000000000000000000", PaymentID: "pay_received", AttemptID: "pat_received",
		Amount: 10000, Currency: "brl", PaymentStatus: "paid",
	})
	response, body = h.postWebhook(payload, true)
	requireStatus(t, response, body, http.StatusAccepted)
	if got := scanCount(t, h.db, "SELECT count(*) FROM webhook_events WHERE provider_event_id = $1 AND status = 'pending'", "evt_e2e_received"); got != 1 {
		t.Fatalf("pending inbox rows = %d, want 1", got)
	}
	if got := scanCount(t, h.db, `SELECT count(*) FROM outbox_events o JOIN webhook_events w ON w.id = o.webhook_event_id WHERE w.provider_event_id = $1`, "evt_e2e_received"); got != 1 {
		t.Fatalf("atomic outbox rows = %d, want 1", got)
	}
	h.waitQueues("receive-only harness leaves broker untouched", 0, 0)

	response, body = h.postWebhook(payload, true)
	requireStatus(t, response, body, http.StatusOK)
	if got := scanCount(t, h.db, "SELECT count(*) FROM webhook_events WHERE provider_event_id = $1", "evt_e2e_received"); got != 1 {
		t.Fatalf("duplicate created %d inbox rows, want 1", got)
	}
	if got := scanCount(t, h.db, `SELECT count(*) FROM outbox_events o JOIN webhook_events w ON w.id = o.webhook_event_id WHERE w.provider_event_id = $1`, "evt_e2e_received"); got != 1 {
		t.Fatalf("duplicate created %d outbox rows, want 1", got)
	}

	unknown := stripeEventPayload(t, stripeEvent{ProviderEventID: "evt_e2e_unknown", Type: "customer.created", SessionID: "cs_unknown"})
	response, body = h.postWebhook(unknown, true)
	requireStatus(t, response, body, http.StatusOK)
	if got := scanCount(t, h.db, "SELECT count(*) FROM webhook_events WHERE provider_event_id = $1 AND status = 'skipped'", "evt_e2e_unknown"); got != 1 {
		t.Fatalf("skipped inbox rows = %d, want 1", got)
	}
	if got := scanCount(t, h.db, `SELECT count(*) FROM outbox_events o JOIN webhook_events w ON w.id = o.webhook_event_id WHERE w.provider_event_id = $1`, "evt_e2e_unknown"); got != 0 {
		t.Fatalf("unknown event created %d outbox rows", got)
	}

	response, body = h.request(http.MethodGet, "/v1/webhook-events?limit=10", apiKeyA, nil, nil)
	requireStatus(t, response, body, http.StatusOK)
	if strings.Contains(string(body), "raw_payload") || strings.Contains(string(body), "rawPayload") ||
		strings.Contains(string(body), `"payload"`) || strings.Contains(string(body), "cs_received") {
		t.Fatalf("operational listing exposed provider payload: %s", body)
	}
	h.waitQueues("ignored event leaves broker untouched", 0, 0)
}

func (h *harness) createOrder(key, idempotencyKey string, requestBody map[string]any) orderResponse {
	h.t.Helper()
	response, body := h.request(http.MethodPost, "/v1/orders", key, requestBody,
		map[string]string{"Idempotency-Key": idempotencyKey})
	requireStatus(h.t, response, body, http.StatusCreated)
	var order orderResponse
	if err := json.Unmarshal(body, &order); err != nil {
		h.t.Fatalf("decode order: %v; body=%s", err, body)
	}
	return order
}

func (h *harness) createCheckout(orderID, idempotencyKey string) checkoutResponse {
	h.t.Helper()
	response, body := h.request(http.MethodPost, "/v1/orders/"+orderID+"/checkout", apiKeyA, nil,
		map[string]string{"Idempotency-Key": idempotencyKey})
	requireStatus(h.t, response, body, http.StatusCreated)
	var checkout checkoutResponse
	if err := json.Unmarshal(body, &checkout); err != nil {
		h.t.Fatalf("decode checkout: %v; body=%s", err, body)
	}
	return checkout
}

type stripeEvent struct {
	ProviderEventID string
	Type            string
	SessionID       string
	OrderID         string
	PaymentID       string
	AttemptID       string
	PaymentIntentID string
	Amount          int64
	Currency        string
	PaymentStatus   string
}

func stripeEventPayload(t *testing.T, event stripeEvent) []byte {
	t.Helper()
	if event.Currency == "" {
		event.Currency = "brl"
	}
	if event.PaymentStatus == "" {
		event.PaymentStatus = "unpaid"
	}
	if event.PaymentIntentID == "" {
		event.PaymentIntentID = "pi_" + event.SessionID
	}
	value := map[string]any{
		"id": event.ProviderEventID, "object": "event", "api_version": stripeadapter.SupportedAPIVersion,
		"created": time.Now().Unix(), "type": event.Type,
		"data": map[string]any{"object": map[string]any{
			"id": event.SessionID, "object": "checkout.session", "payment_status": event.PaymentStatus,
			"status": "complete", "amount_total": event.Amount, "currency": event.Currency,
			"client_reference_id": event.OrderID,
			"payment_intent":      map[string]any{"id": event.PaymentIntentID, "object": "payment_intent"},
			"metadata":            map[string]string{"order_id": event.OrderID, "payment_id": event.PaymentID, "payment_attempt_id": event.AttemptID},
		}},
	}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode Stripe event: %v", err)
	}
	return payload
}

func (h *harness) paymentReferences(orderID string) (paymentID, attemptID, sessionID string, amount int64, currency string) {
	h.t.Helper()
	err := h.db.QueryRowContext(context.Background(), `
		SELECT p.id, a.id, a.provider_session_id, p.amount, p.currency
		FROM payments p JOIN payment_attempts a ON a.payment_id = p.id
		WHERE p.order_id = $1 ORDER BY a.created_at DESC LIMIT 1`, orderID).
		Scan(&paymentID, &attemptID, &sessionID, &amount, &currency)
	if err != nil {
		h.t.Fatalf("read payment references for %s: %v", orderID, err)
	}
	return
}
