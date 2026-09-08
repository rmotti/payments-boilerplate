//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rmotti/payments-boilerplate/internal/adapters/rabbitmq"
)

type aggregateState struct {
	Order, Payment, Attempt, Inbox, Outbox string
	InboxAttempts, OutboxAttempts          int
}

func TestCardAndPixReachTheirExpectedTerminalStates(t *testing.T) {
	h := newHarness(t, true)

	cardOrder := h.createOrder(apiKeyA, "card-order", map[string]any{"productId": "product_demo", "quantity": 1})
	h.createCheckout(cardOrder.ID, "card-checkout")
	cardPayment, cardAttempt, cardSession, amount, currency := h.paymentReferences(cardOrder.ID)
	cardEvent := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_card_paid", Type: "checkout.session.completed", SessionID: cardSession,
		OrderID: cardOrder.ID, PaymentID: cardPayment, AttemptID: cardAttempt,
		Amount: amount, Currency: currency, PaymentStatus: "paid",
	})
	response, body := h.postWebhook(cardEvent, true)
	requireStatus(t, response, body, http.StatusAccepted)
	h.waitAggregate("card paid", cardOrder.ID, "evt_e2e_card_paid", func(state aggregateState) bool {
		return state.Order == "paid" && state.Payment == "succeeded" && state.Attempt == "succeeded" &&
			state.Inbox == "processed" && state.Outbox == "published"
	})
	h.assertPipelineCounts("evt_e2e_card_paid", 1, 1)

	// Re-deliver the exact committed inbox reference through RabbitMQ. A
	// second delivery is allowed; the consumer must observe processed and make
	// it a no-op without changing any final state.
	h.republishStoredEvent("evt_e2e_card_paid")
	h.waitQueues("card redelivery drained", 0, 0)
	state := h.readAggregate(cardOrder.ID, "evt_e2e_card_paid")
	if state.Order != "paid" || state.Payment != "succeeded" || state.Attempt != "succeeded" || state.InboxAttempts != 1 {
		t.Fatalf("redelivery changed committed effect: %#v", state)
	}

	pixOrder := h.createOrder(apiKeyA, "pix-order", map[string]any{"productId": "product_demo", "quantity": 1})
	h.createCheckout(pixOrder.ID, "pix-checkout")
	pixPayment, pixAttempt, pixSession, pixAmount, pixCurrency := h.paymentReferences(pixOrder.ID)
	completed := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_pix_completed", Type: "checkout.session.completed", SessionID: pixSession,
		OrderID: pixOrder.ID, PaymentID: pixPayment, AttemptID: pixAttempt,
		Amount: pixAmount, Currency: pixCurrency, PaymentStatus: "unpaid",
	})
	response, body = h.postWebhook(completed, true)
	requireStatus(t, response, body, http.StatusAccepted)
	h.waitAggregate("Pix awaiting settlement", pixOrder.ID, "evt_e2e_pix_completed", func(state aggregateState) bool {
		return state.Order == "pending" && state.Payment == "processing" && state.Attempt == "pending" && state.Inbox == "processed"
	})

	succeeded := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_pix_succeeded", Type: "checkout.session.async_payment_succeeded", SessionID: pixSession,
		OrderID: pixOrder.ID, PaymentID: pixPayment, AttemptID: pixAttempt,
		Amount: pixAmount, Currency: pixCurrency, PaymentStatus: "paid",
	})
	response, body = h.postWebhook(succeeded, true)
	requireStatus(t, response, body, http.StatusAccepted)
	h.waitAggregate("Pix settled", pixOrder.ID, "evt_e2e_pix_succeeded", func(state aggregateState) bool {
		return state.Order == "paid" && state.Payment == "succeeded" && state.Attempt == "succeeded" && state.Inbox == "processed"
	})

	lateFailure := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_pix_late_failure", Type: "checkout.session.async_payment_failed", SessionID: pixSession,
		OrderID: pixOrder.ID, PaymentID: pixPayment, AttemptID: pixAttempt,
		Amount: pixAmount, Currency: pixCurrency, PaymentStatus: "unpaid",
	})
	response, body = h.postWebhook(lateFailure, true)
	requireStatus(t, response, body, http.StatusAccepted)
	h.waitAggregate("late negative event", pixOrder.ID, "evt_e2e_pix_late_failure", func(state aggregateState) bool {
		return state.Order == "paid" && state.Payment == "succeeded" && state.Attempt == "succeeded" && state.Inbox == "processed"
	})
	h.waitQueues("Pix pipeline drained", 0, 0)
}

func TestExpiredCheckoutCreatesANewAttempt(t *testing.T) {
	h := newHarness(t, true)
	order := h.createOrder(apiKeyA, "expiry-order", map[string]any{"productId": "product_demo", "quantity": 1})
	firstCheckout := h.createCheckout(order.ID, "expiry-checkout-one")
	paymentID, attemptID, sessionID, amount, currency := h.paymentReferences(order.ID)
	expired := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_expired", Type: "checkout.session.expired", SessionID: sessionID,
		OrderID: order.ID, PaymentID: paymentID, AttemptID: attemptID,
		Amount: amount, Currency: currency, PaymentStatus: "unpaid",
	})
	response, body := h.postWebhook(expired, true)
	requireStatus(t, response, body, http.StatusAccepted)
	h.waitAggregate("checkout expiration", order.ID, "evt_e2e_expired", func(state aggregateState) bool {
		return state.Order == "pending" && state.Payment == "pending" && state.Attempt == "expired" && state.Inbox == "processed"
	})

	secondCheckout := h.createCheckout(order.ID, "expiry-checkout-two")
	if secondCheckout.CheckoutURL == firstCheckout.CheckoutURL {
		t.Fatalf("new attempt reused expired checkout URL %q", firstCheckout.CheckoutURL)
	}
	if got := scanCount(t, h.db, "SELECT count(*) FROM payment_attempts WHERE payment_id = $1", paymentID); got != 2 {
		t.Fatalf("attempts after expiration = %d, want 2", got)
	}
	requests, _ := h.provider.snapshot()
	if len(requests) != 2 {
		t.Fatalf("provider calls after expiration = %d, want 2", len(requests))
	}
	h.waitQueues("expiration pipeline drained", 0, 0)
}

func TestMismatchFailsInboxAndProducesConfirmedDeadLetter(t *testing.T) {
	h := newHarness(t, true)
	order := h.createOrder(apiKeyA, "mismatch-order", map[string]any{"productId": "product_demo", "quantity": 1})
	h.createCheckout(order.ID, "mismatch-checkout")
	paymentID, attemptID, sessionID, amount, currency := h.paymentReferences(order.ID)
	mismatch := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_amount_mismatch", Type: "checkout.session.completed", SessionID: sessionID,
		OrderID: order.ID, PaymentID: paymentID, AttemptID: attemptID,
		Amount: amount + 1, Currency: currency, PaymentStatus: "paid",
	})
	response, body := h.postWebhook(mismatch, true)
	requireStatus(t, response, body, http.StatusAccepted)
	h.waitAggregate("terminal mismatch", order.ID, "evt_e2e_amount_mismatch", func(state aggregateState) bool {
		return state.Order == "pending" && state.Payment == "pending" && state.Attempt == "pending" &&
			state.Inbox == "failed" && state.Outbox == "published"
	})
	h.waitQueues("confirmed dead letter", 0, 1)
	var lastError string
	if err := h.db.QueryRowContext(context.Background(), "SELECT last_error FROM webhook_events WHERE provider_event_id = $1", "evt_e2e_amount_mismatch").Scan(&lastError); err != nil {
		t.Fatalf("read mismatch error: %v", err)
	}
	if lastError == "" {
		t.Fatal("failed mismatch has no diagnostic")
	}
}

func TestConcurrentEventsSerializeOnOnePayment(t *testing.T) {
	// Receive both events before starting the worker. Both outbox rows then
	// become available together to a consumer configured with concurrency 2,
	// without calling testing.Fatal from an auxiliary goroutine.
	h := newHarness(t, false)
	order := h.createOrder(apiKeyA, "concurrent-order", map[string]any{"productId": "product_demo", "quantity": 1})
	h.createCheckout(order.ID, "concurrent-checkout")
	paymentID, attemptID, sessionID, amount, currency := h.paymentReferences(order.ID)
	events := []stripeEvent{
		{ProviderEventID: "evt_e2e_concurrent_success", Type: "checkout.session.async_payment_succeeded", PaymentStatus: "paid"},
		{ProviderEventID: "evt_e2e_concurrent_failure", Type: "checkout.session.async_payment_failed", PaymentStatus: "unpaid"},
	}
	for index := range events {
		events[index].SessionID, events[index].OrderID = sessionID, order.ID
		events[index].PaymentID, events[index].AttemptID = paymentID, attemptID
		events[index].Amount, events[index].Currency = amount, currency
	}

	for _, event := range events {
		payload := stripeEventPayload(t, event)
		response, body := h.postWebhook(payload, true)
		requireStatus(t, response, body, http.StatusAccepted)
	}
	h.startWorker()
	h.poll("concurrent terminal events", func() (bool, string, error) {
		var orderStatus, paymentStatus, attemptStatus string
		var closed int
		err := h.db.QueryRowContext(context.Background(), `
			SELECT o.status, p.status, a.status,
			       (SELECT count(*) FROM webhook_events WHERE provider_event_id IN ($2, $3) AND status IN ('processed','failed'))
			FROM orders o JOIN payments p ON p.order_id=o.id JOIN payment_attempts a ON a.payment_id=p.id
			WHERE o.id=$1`, order.ID, events[0].ProviderEventID, events[1].ProviderEventID).
			Scan(&orderStatus, &paymentStatus, &attemptStatus, &closed)
		if err != nil {
			return false, "", err
		}
		consistentSuccess := orderStatus == "paid" && paymentStatus == "succeeded" && attemptStatus == "succeeded"
		consistentFailure := orderStatus == "pending" && paymentStatus == "failed" && attemptStatus == "failed"
		return closed == 2 && (consistentSuccess || consistentFailure),
			fmt.Sprintf("order=%s payment=%s attempt=%s closed=%d", orderStatus, paymentStatus, attemptStatus, closed), nil
	})
	if got := scanCount(t, h.db, "SELECT count(*) FROM webhook_events WHERE provider_event_id IN ($1,$2)", events[0].ProviderEventID, events[1].ProviderEventID); got != 2 {
		t.Fatalf("concurrent inbox rows = %d, want 2", got)
	}
	failed := scanCount(t, h.db, "SELECT count(*) FROM webhook_events WHERE provider_event_id IN ($1,$2) AND status='failed'", events[0].ProviderEventID, events[1].ProviderEventID)
	h.waitQueues("concurrent pipeline settled", 0, failed)
}

func TestFailedEventReprocessReusesInboxAndOutbox(t *testing.T) {
	h := newHarness(t, false)
	order := h.createOrder(apiKeyA, "reprocess-order", map[string]any{"productId": "product_demo", "quantity": 1})
	h.createCheckout(order.ID, "reprocess-checkout")
	paymentID, attemptID, sessionID, amount, currency := h.paymentReferences(order.ID)
	payload := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_reprocess", Type: "checkout.session.completed", SessionID: sessionID,
		OrderID: order.ID, PaymentID: paymentID, AttemptID: attemptID,
		Amount: amount, Currency: currency, PaymentStatus: "paid",
	})
	response, body := h.postWebhook(payload, true)
	requireStatus(t, response, body, http.StatusAccepted)

	var eventID, outboxID string
	if err := h.db.QueryRowContext(context.Background(), `SELECT w.id, o.id FROM webhook_events w JOIN outbox_events o ON o.webhook_event_id=w.id WHERE w.provider_event_id=$1`, "evt_e2e_reprocess").Scan(&eventID, &outboxID); err != nil {
		t.Fatalf("read replay identifiers: %v", err)
	}
	// Simulate a previously classified transient failure after the durable
	// receive. Reprocessing itself is what is under test; mutating the provider
	// payload would make a replay non-deterministic and is intentionally avoided.
	if _, err := h.db.ExecContext(context.Background(), `UPDATE webhook_events SET status='failed', attempts=2, last_error='e2e transient failure' WHERE id=$1`, eventID); err != nil {
		t.Fatalf("seed failed inbox: %v", err)
	}
	if _, err := h.db.ExecContext(context.Background(), `UPDATE outbox_events SET status='failed', attempts=2, last_error='e2e transient failure', published_at=NULL, locked_until=NULL, locked_by=NULL WHERE id=$1`, outboxID); err != nil {
		t.Fatalf("seed failed outbox: %v", err)
	}

	response, body = h.request(http.MethodPost, "/v1/webhook-events/"+eventID+"/reprocess", apiKeyA, nil, nil)
	requireStatus(t, response, body, http.StatusAccepted)
	if scanCount(t, h.db, "SELECT count(*) FROM webhook_events WHERE provider_event_id=$1", "evt_e2e_reprocess") != 1 ||
		scanCount(t, h.db, "SELECT count(*) FROM outbox_events WHERE webhook_event_id=$1", eventID) != 1 {
		t.Fatal("reprocess created a second durable row")
	}
	var replayCount int
	var currentOutboxID, eventStatus, outboxStatus string
	if err := h.db.QueryRowContext(context.Background(), `SELECT w.replay_count, w.status, o.id, o.status FROM webhook_events w JOIN outbox_events o ON o.webhook_event_id=w.id WHERE w.id=$1`, eventID).
		Scan(&replayCount, &eventStatus, &currentOutboxID, &outboxStatus); err != nil {
		t.Fatalf("read replayed rows: %v", err)
	}
	if replayCount != 1 || eventStatus != "pending" || outboxStatus != "pending" || currentOutboxID != outboxID {
		t.Fatalf("replayed state count=%d inbox=%s outbox=%s id=%s, original=%s", replayCount, eventStatus, outboxStatus, currentOutboxID, outboxID)
	}

	h.startWorker()
	h.waitAggregate("reprocessed event", order.ID, "evt_e2e_reprocess", func(state aggregateState) bool {
		return state.Order == "paid" && state.Payment == "succeeded" && state.Attempt == "succeeded" &&
			state.Inbox == "processed" && state.Outbox == "published"
	})
	h.waitQueues("reprocessed pipeline drained", 0, 0)
}

func (h *harness) readAggregate(orderID, providerEventID string) aggregateState {
	h.t.Helper()
	var state aggregateState
	err := h.db.QueryRowContext(context.Background(), `
		SELECT o.status, p.status, a.status, w.status, ob.status, w.attempts, ob.attempts
		FROM orders o
		JOIN payments p ON p.order_id=o.id
		JOIN payment_attempts a ON a.payment_id=p.id
		JOIN webhook_events w ON w.provider_event_id=$2
		JOIN outbox_events ob ON ob.webhook_event_id=w.id
		WHERE o.id=$1 ORDER BY a.created_at DESC LIMIT 1`, orderID, providerEventID).
		Scan(&state.Order, &state.Payment, &state.Attempt, &state.Inbox, &state.Outbox, &state.InboxAttempts, &state.OutboxAttempts)
	if err != nil && err != sql.ErrNoRows {
		h.t.Fatalf("read aggregate: %v", err)
	}
	return state
}

func (h *harness) waitAggregate(description, orderID, providerEventID string, satisfied func(aggregateState) bool) {
	h.t.Helper()
	h.poll(description+" order="+orderID+" provider_event="+providerEventID, func() (bool, string, error) {
		var state aggregateState
		err := h.db.QueryRowContext(context.Background(), `
			SELECT o.status, p.status, a.status, w.status, ob.status, w.attempts, ob.attempts
			FROM orders o
			JOIN payments p ON p.order_id=o.id
			JOIN payment_attempts a ON a.payment_id=p.id
			JOIN webhook_events w ON w.provider_event_id=$2
			JOIN outbox_events ob ON ob.webhook_event_id=w.id
			WHERE o.id=$1 ORDER BY a.created_at DESC LIMIT 1`, orderID, providerEventID).
			Scan(&state.Order, &state.Payment, &state.Attempt, &state.Inbox, &state.Outbox, &state.InboxAttempts, &state.OutboxAttempts)
		if err == sql.ErrNoRows {
			return false, "rows not visible yet", nil
		}
		if err != nil {
			return false, "", err
		}
		return satisfied(state), fmt.Sprintf("%+v", state), nil
	})
}

func (h *harness) assertPipelineCounts(providerEventID string, inbox, outbox int) {
	h.t.Helper()
	if got := scanCount(h.t, h.db, "SELECT count(*) FROM webhook_events WHERE provider_event_id=$1", providerEventID); got != inbox {
		h.t.Fatalf("inbox rows for %s = %d, want %d", providerEventID, got, inbox)
	}
	if got := scanCount(h.t, h.db, `SELECT count(*) FROM outbox_events o JOIN webhook_events w ON w.id=o.webhook_event_id WHERE w.provider_event_id=$1`, providerEventID); got != outbox {
		h.t.Fatalf("outbox rows for %s = %d, want %d", providerEventID, got, outbox)
	}
}

func (h *harness) queueDepths() (main, dead int, err error) {
	connection, err := amqp.Dial(h.rabbitURL)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = connection.Close() }()
	channel, err := connection.Channel()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = channel.Close() }()
	mainQueue, err := channel.QueueDeclarePassive(rabbitmq.WebhooksQueue, true, false, false, false, nil)
	if err != nil {
		return 0, 0, err
	}
	deadQueue, err := channel.QueueDeclarePassive(rabbitmq.DeadLetterQueue, true, false, false, false, nil)
	if err != nil {
		return 0, 0, err
	}
	return mainQueue.Messages, deadQueue.Messages, nil
}

func (h *harness) waitQueues(description string, main, dead int) {
	h.t.Helper()
	h.poll(description, func() (bool, string, error) {
		gotMain, gotDead, err := h.queueDepths()
		return gotMain == main && gotDead == dead, fmt.Sprintf("main=%d dead=%d", gotMain, gotDead), err
	})
}

func (h *harness) republishStoredEvent(providerEventID string) {
	h.t.Helper()
	var eventID, messageID, eventType, routingKey, correlationID string
	var schemaVersion int
	var occurredAt time.Time
	err := h.db.QueryRowContext(context.Background(), `
		SELECT w.id, o.id, o.event_type, o.schema_version, o.routing_key, o.correlation_id, o.occurred_at
		FROM webhook_events w JOIN outbox_events o ON o.webhook_event_id=w.id
		WHERE w.provider_event_id=$1`, providerEventID).
		Scan(&eventID, &messageID, &eventType, &schemaVersion, &routingKey, &correlationID, &occurredAt)
	if err != nil {
		h.t.Fatalf("read event for redelivery: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"messageId": messageID, "type": eventType, "schemaVersion": schemaVersion,
		"occurredAt": occurredAt, "correlationId": correlationID, "webhookEventId": eventID,
	})
	if err != nil {
		h.t.Fatalf("encode redelivery: %v", err)
	}
	connection, err := amqp.Dial(h.rabbitURL)
	if err != nil {
		h.t.Fatalf("connect for redelivery: %v", err)
	}
	defer func() { _ = connection.Close() }()
	channel, err := connection.Channel()
	if err != nil {
		h.t.Fatalf("open redelivery channel: %v", err)
	}
	defer func() { _ = channel.Close() }()
	if err := channel.Confirm(false); err != nil {
		h.t.Fatalf("enable redelivery confirms: %v", err)
	}
	confirmation, err := channel.PublishWithDeferredConfirmWithContext(context.Background(), rabbitmq.EventsExchange, routingKey, true, false, amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: messageID,
		Type: eventType, CorrelationId: correlationID, Timestamp: occurredAt, Body: body,
	})
	if err != nil {
		h.t.Fatalf("publish redelivery: %v", err)
	}
	confirmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	confirmed, err := confirmation.WaitContext(confirmCtx)
	if err != nil || !confirmed {
		h.t.Fatalf("confirm redelivery: confirmed=%v err=%v", confirmed, err)
	}
}
