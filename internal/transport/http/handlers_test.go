package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	app "github.com/rmotti/payments-boilerplate/internal/application/orders"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	webhookdomain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
	"go.uber.org/zap"
)

func TestGetHealthReady(t *testing.T) {
	t.Parallel()

	handler := NewAPIHandler(health.New("payments-api", "test", map[string]health.Checker{
		"postgres": func(context.Context) error { return nil },
	}), nil, nil, nil)

	response, err := handler.GetHealth(context.Background(), openapi.GetHealthRequestObject{})
	if err != nil {
		t.Fatalf("GetHealth() error = %v", err)
	}
	ready, ok := response.(openapi.GetHealth200JSONResponse)
	if !ok {
		t.Fatalf("GetHealth() response type = %T, want 200 response", response)
	}
	if ready.Body.Status != openapi.Ok || ready.Body.Checks["postgres"] != openapi.Up {
		t.Fatalf("GetHealth() response = %#v, want ready", ready)
	}
}

func TestGetHealthUnavailable(t *testing.T) {
	t.Parallel()

	handler := NewAPIHandler(health.New("payments-worker", "test", map[string]health.Checker{
		"rabbitmq": func(context.Context) error { return errors.New("unavailable") },
	}), nil, nil, nil)

	response, err := handler.GetHealth(context.Background(), openapi.GetHealthRequestObject{})
	if err != nil {
		t.Fatalf("GetHealth() error = %v", err)
	}
	unavailable, ok := response.(openapi.GetHealth503JSONResponse)
	if !ok {
		t.Fatalf("GetHealth() response type = %T, want 503 response", response)
	}
	if unavailable.Body.Status != openapi.Unavailable || unavailable.Body.Checks["rabbitmq"] != openapi.Down {
		t.Fatalf("GetHealth() response = %#v, want unavailable", unavailable)
	}
}

// stubOrders answers Create with a canned order or error and records the
// input it received.
type stubOrders struct {
	order domain.Order
	err   error
	got   app.CreateInput
	calls int
}

func (s *stubOrders) Create(_ context.Context, in app.CreateInput) (domain.Order, error) {
	s.calls++
	s.got = in
	if s.err != nil {
		return domain.Order{}, s.err
	}
	return s.order, nil
}

func (s *stubOrders) Get(_ context.Context, _ string) (domain.Order, error) {
	s.calls++
	if s.err != nil {
		return domain.Order{}, s.err
	}
	return s.order, nil
}

// realOrders runs the actual use case over in-memory ports so the handler is
// exercised together with validation and pricing.
func realOrders(t *testing.T) *app.Service {
	t.Helper()
	catalog := catalogFunc(func(_ context.Context, id string) (domain.Product, error) {
		if id != "product_demo" {
			return domain.Product{}, app.ErrProductNotFound
		}
		return domain.Product{ID: id, UnitAmount: 10000, Currency: domain.BRL}, nil
	})
	return app.NewService(catalog, &repoStub{},
		app.WithClock(func() time.Time { return time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC) }),
		app.WithIDGenerator(func() (string, error) { return "ord_fixed", nil }),
	)
}

type catalogFunc func(ctx context.Context, id string) (domain.Product, error)

func (f catalogFunc) Product(ctx context.Context, id string) (domain.Product, error) {
	return f(ctx, id)
}

type repoStub struct{}

func (*repoStub) CreateOrGet(_ context.Context, order domain.Order) (domain.Order, bool, error) {
	return order, true, nil
}

func (*repoStub) Get(context.Context, string) (domain.Order, error) {
	return domain.Order{}, app.ErrOrderNotFound
}

func newTestHandler(orders OrderCreator) http.Handler {
	return newTestHandlerWithCheckout(orders, nil)
}

func newTestHandlerWithCheckout(orders OrderCreator, checkouts CheckoutCreator) http.Handler {
	return newTestHandlerWith(orders, checkouts, nil)
}

func newTestHandlerWith(orders OrderCreator, checkouts CheckoutCreator, webhooks WebhookReceiver) http.Handler {
	return newTestHandlerWithOperations(orders, checkouts, webhooks, nil)
}

func newTestHandlerWithOperations(
	orders OrderCreator,
	checkouts CheckoutCreator,
	webhooks WebhookReceiver,
	operations WebhookOperations,
) http.Handler {
	api := NewAPIHandler(health.New("payments-api", "test", nil), orders, checkouts, webhooks, operations)
	verifier := verifierFunc(func(candidate string) bool { return candidate == testAPIKey })
	return New(Config{Address: ":0", ShutdownTimeout: time.Second}, zap.NewNop(), api, verifier).server.Handler
}

type webhookOperationsStub struct {
	items      []webhookapp.EventInspection
	err        error
	gotStatus  webhookdomain.Status
	gotLimit   int
	gotEventID string
}

func (s *webhookOperationsStub) List(
	_ context.Context,
	status webhookdomain.Status,
	limit int,
) ([]webhookapp.EventInspection, error) {
	s.gotStatus, s.gotLimit = status, limit
	return s.items, s.err
}

func (s *webhookOperationsStub) Reprocess(_ context.Context, eventID string) (webhookapp.EventInspection, error) {
	s.gotEventID = eventID
	if s.err != nil {
		return webhookapp.EventInspection{}, s.err
	}
	return s.items[0], nil
}

func TestListWebhookEventsIsAuthenticatedAndOmitsPayloads(t *testing.T) {
	t.Parallel()

	receivedAt := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	operations := &webhookOperationsStub{items: []webhookapp.EventInspection{{
		ID: "evt_0123456789abcdef0123456789abcdef", Provider: webhookdomain.Stripe,
		ProviderEventID: "evt_provider", EventType: "checkout.session.completed",
		Status: webhookdomain.StatusFailed, Attempts: 3, ReceivedAt: receivedAt,
		UpdatedAt: receivedAt, LastError: "bad event shape", ReplayCount: 0,
		Outbox: &webhookapp.OutboxInspection{
			ID: "msg_0123456789abcdef0123456789abcdef", Status: webhookdomain.MessagePublished,
			Attempts: 1, NextAttemptAt: receivedAt,
		},
	}}}
	handler := newTestHandlerWithOperations(nil, nil, nil, operations)

	unauthorized := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/webhook-events", nil)
	unauthorizedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthorizedRecorder.Code)
	}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/webhook-events?status=failed&limit=10", nil)
	request.Header.Set(apiKeyHeader, testAPIKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if operations.gotStatus != webhookdomain.StatusFailed || operations.gotLimit != 10 {
		t.Fatalf("operation input = %q/%d, want failed/10", operations.gotStatus, operations.gotLimit)
	}
	if strings.Contains(recorder.Body.String(), `"payload":`) || strings.Contains(recorder.Body.String(), `"rawPayload":`) ||
		!strings.Contains(recorder.Body.String(), "bad event shape") {
		t.Fatalf("body = %s, want diagnostics without provider payload", recorder.Body.String())
	}
}

func TestReprocessWebhookEvent(t *testing.T) {
	t.Parallel()

	eventID := "evt_0123456789abcdef0123456789abcdef"
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	operations := &webhookOperationsStub{items: []webhookapp.EventInspection{{
		ID: eventID, Provider: webhookdomain.Stripe, ProviderEventID: "evt_provider",
		EventType: "checkout.session.completed", Status: webhookdomain.StatusPending,
		ReceivedAt: now, UpdatedAt: now, ReplayCount: 1,
	}}}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/webhook-events/"+eventID+"/reprocess", nil)
	request.Header.Set(apiKeyHeader, testAPIKey)
	recorder := httptest.NewRecorder()
	newTestHandlerWithOperations(nil, nil, nil, operations).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", recorder.Code, recorder.Body.String())
	}
	if operations.gotEventID != eventID {
		t.Fatalf("event id = %q, want %q", operations.gotEventID, eventID)
	}
}

func TestReprocessWebhookEventRejectsNonFailedWork(t *testing.T) {
	t.Parallel()

	eventID := "evt_0123456789abcdef0123456789abcdef"
	operations := &webhookOperationsStub{err: webhookapp.ErrEventNotReplayable}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/webhook-events/"+eventID+"/reprocess", nil)
	request.Header.Set(apiKeyHeader, testAPIKey)
	recorder := httptest.NewRecorder()
	newTestHandlerWithOperations(nil, nil, nil, operations).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", recorder.Code, recorder.Body.String())
	}
	if body := decodeError(t, recorder); body.Code != codeWebhookEventNotReplayable {
		t.Fatalf("error code = %q, want %q", body.Code, codeWebhookEventNotReplayable)
	}
}

type stubCheckouts struct {
	checkout paymentapp.Checkout
	err      error
	got      paymentapp.CreateInput
	calls    int
}

func (s *stubCheckouts) CreateCheckout(_ context.Context, input paymentapp.CreateInput) (paymentapp.Checkout, error) {
	s.calls++
	s.got = input
	return s.checkout, s.err
}

func TestGetOrder(t *testing.T) {
	t.Parallel()

	orderID := "ord_0123456789abcdef0123456789abcdef"
	orders := &stubOrders{order: domain.Order{ID: orderID, Status: domain.StatusPending, Amount: 10000, Currency: domain.BRL}}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/orders/"+orderID, nil)
	request.Header.Set(apiKeyHeader, testAPIKey)
	recorder := httptest.NewRecorder()
	newTestHandler(orders).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	var body openapi.Order
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	if body.Id != orderID || body.Amount != 10000 || body.Currency != "BRL" {
		t.Fatalf("body = %#v, want persisted order", body)
	}
}

func TestGetOrderNotFound(t *testing.T) {
	t.Parallel()

	orderID := "ord_0123456789abcdef0123456789abcdef"
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/orders/"+orderID, nil)
	request.Header.Set(apiKeyHeader, testAPIKey)
	recorder := httptest.NewRecorder()
	newTestHandler(&stubOrders{err: app.ErrOrderNotFound}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", recorder.Code, recorder.Body.String())
	}
	if body := decodeError(t, recorder); body.Code != codeOrderNotFound {
		t.Fatalf("error = %#v, want %q", body, codeOrderNotFound)
	}
}

func TestCreateCheckout(t *testing.T) {
	t.Parallel()

	orderID := "ord_0123456789abcdef0123456789abcdef"
	expires := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	checkouts := &stubCheckouts{checkout: paymentapp.Checkout{
		URL: "https://checkout.stripe.com/c/pay/test", ExpiresAt: expires,
	}}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders/"+orderID+"/checkout", nil)
	request.Header.Set(apiKeyHeader, testAPIKey)
	request.Header.Set("Idempotency-Key", "checkout-key")
	recorder := httptest.NewRecorder()
	newTestHandlerWithCheckout(nil, checkouts).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
	}
	if checkouts.got != (paymentapp.CreateInput{OrderID: orderID, IdempotencyKey: "checkout-key"}) {
		t.Fatalf("checkout input = %#v, want path and idempotency key", checkouts.got)
	}
	var body openapi.Checkout
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode checkout: %v", err)
	}
	if body.CheckoutUrl != checkouts.checkout.URL || !body.ExpiresAt.Equal(expires) {
		t.Fatalf("body = %#v, want checkout response", body)
	}
}

func TestCreateCheckoutExpectedErrors(t *testing.T) {
	t.Parallel()

	orderID := "ord_0123456789abcdef0123456789abcdef"
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "order missing", err: app.ErrOrderNotFound, status: http.StatusNotFound, code: codeOrderNotFound},
		{name: "key conflict", err: paymentapp.ErrIdempotencyKeyConflict, status: http.StatusConflict, code: codeIdempotencyKeyConflict},
		{name: "checkout active", err: paymentapp.ErrCheckoutInProgress, status: http.StatusConflict, code: codeCheckoutInProgress},
		{name: "order not payable", err: paymentapp.ErrOrderNotPayable, status: http.StatusConflict, code: codeOrderNotPayable},
		{name: "provider unavailable", err: paymentapp.ErrProviderUnavailable, status: http.StatusBadGateway, code: codeProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders/"+orderID+"/checkout", nil)
			request.Header.Set(apiKeyHeader, testAPIKey)
			request.Header.Set("Idempotency-Key", "checkout-key")
			recorder := httptest.NewRecorder()
			newTestHandlerWithCheckout(nil, &stubCheckouts{err: tt.err}).ServeHTTP(recorder, request)
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tt.status, recorder.Body.String())
			}
			if body := decodeError(t, recorder); body.Code != tt.code {
				t.Fatalf("error = %#v, want %q", body, tt.code)
			}
		})
	}
}

const testAPIKey = "test-api-key-with-at-least-32-characters"

func postOrder(t *testing.T, handler http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(apiKeyHeader, testAPIKey)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestCreateOrderRequiresAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{name: "missing"},
		{name: "invalid", key: "invalid-api-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			orders := &stubOrders{}
			recorder := postOrder(t, newTestHandler(orders), `{"productId":"product_demo","quantity":1}`, map[string]string{
				"Idempotency-Key": "key-1",
				apiKeyHeader:      tt.key,
			})

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", recorder.Code, recorder.Body.String())
			}
			body := decodeError(t, recorder)
			if body.Code != codeUnauthorized || body.Message != ErrUnauthorized.Error() {
				t.Fatalf("error = %#v, want a generic unauthorized error", body)
			}
			if orders.calls != 0 {
				t.Fatalf("order service calls = %d, want none", orders.calls)
			}
		})
	}
}

func TestHealthRemainsPublic(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(nil)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
}

func decodeError(t *testing.T, recorder *httptest.ResponseRecorder) openapi.Error {
	t.Helper()
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body openapi.Error
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, recorder.Body.String())
	}
	return body
}

func TestCreateOrderReturnsServerComputedAmount(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(realOrders(t))
	recorder := postOrder(t, handler, `{"productId":"product_demo","quantity":3}`, map[string]string{
		"Idempotency-Key":  "key-1",
		"X-Correlation-ID": "corr-1",
	})

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
	}
	var body openapi.Order
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := openapi.Order{Id: "ord_fixed", Status: openapi.OrderStatusPending, Amount: 30000, Currency: "BRL"}
	if body != want {
		t.Fatalf("body = %#v, want %#v", body, want)
	}
	if got := recorder.Header().Get("X-Correlation-ID"); got != "corr-1" {
		t.Fatalf("X-Correlation-ID = %q, want corr-1", got)
	}
}

func TestCreateOrderIgnoresClientAmount(t *testing.T) {
	t.Parallel()

	stub := &stubOrders{order: domain.Order{ID: "ord_1", Status: domain.StatusPending, Amount: 10000, Currency: domain.BRL}}
	handler := newTestHandler(stub)
	recorder := postOrder(t, handler, `{"productId":"product_demo","quantity":1,"amount":1,"currency":"USD"}`, map[string]string{
		"Idempotency-Key": "key-1",
	})

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
	}
	if stub.got != (app.CreateInput{ProductID: "product_demo", Quantity: 1, IdempotencyKey: "key-1"}) {
		t.Fatalf("use case input = %#v, want only product, quantity and key", stub.got)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"amount":10000`)) {
		t.Fatalf("body = %s, want the server amount", recorder.Body.String())
	}
}

func TestCreateOrderRequestErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		headers    map[string]string
		wantStatus int
		wantCode   string
		wantSubstr string
	}{
		{
			name:       "missing idempotency key",
			body:       `{"productId":"product_demo","quantity":1}`,
			headers:    map[string]string{},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
			wantSubstr: "Idempotency-Key",
		},
		{
			name:       "malformed json",
			body:       `{"productId":`,
			headers:    map[string]string{"Idempotency-Key": "k"},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
			wantSubstr: "decode",
		},
		{
			name:       "fractional quantity",
			body:       `{"productId":"product_demo","quantity":1.5}`,
			headers:    map[string]string{"Idempotency-Key": "k"},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
			wantSubstr: "quantity",
		},
		{
			name:       "zero quantity",
			body:       `{"productId":"product_demo","quantity":0}`,
			headers:    map[string]string{"Idempotency-Key": "k"},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
			wantSubstr: "quantity must be between 1 and 1000",
		},
		{
			name:       "quantity above limit",
			body:       `{"productId":"product_demo","quantity":1001}`,
			headers:    map[string]string{"Idempotency-Key": "k"},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
			wantSubstr: "quantity",
		},
		{
			name:       "empty product",
			body:       `{"productId":"","quantity":1}`,
			headers:    map[string]string{"Idempotency-Key": "k"},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
			wantSubstr: "product id",
		},
		{
			name:       "oversized idempotency key",
			body:       `{"productId":"product_demo","quantity":1}`,
			headers:    map[string]string{"Idempotency-Key": strings.Repeat("k", 256)},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
			wantSubstr: "idempotency key",
		},
		{
			name:       "unknown product",
			body:       `{"productId":"product_missing","quantity":1}`,
			headers:    map[string]string{"Idempotency-Key": "k"},
			wantStatus: http.StatusNotFound,
			wantCode:   codeProductNotFound,
			wantSubstr: "product not found",
		},
		{
			name:       "oversized body",
			body:       `{"productId":"` + strings.Repeat("a", maxBodyBytes) + `","quantity":1}`,
			headers:    map[string]string{"Idempotency-Key": "k"},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   codeInvalidRequest,
			wantSubstr: "too large",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recorder := postOrder(t, newTestHandler(realOrders(t)), tt.body, tt.headers)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			body := decodeError(t, recorder)
			if body.Code != tt.wantCode || !strings.Contains(body.Message, tt.wantSubstr) {
				t.Fatalf("error = %#v, want code %q containing %q", body, tt.wantCode, tt.wantSubstr)
			}
			if body.CorrelationId == "" || body.CorrelationId != recorder.Header().Get("X-Correlation-ID") {
				t.Fatalf("correlationId = %q, want the X-Correlation-ID header %q", body.CorrelationId, recorder.Header().Get("X-Correlation-ID"))
			}
		})
	}
}

func TestCreateOrderIdempotencyKeyConflict(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(&stubOrders{err: app.ErrIdempotencyKeyConflict})
	recorder := postOrder(t, handler, `{"productId":"product_demo","quantity":1}`, map[string]string{"Idempotency-Key": "k"})

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", recorder.Code, recorder.Body.String())
	}
	if body := decodeError(t, recorder); body.Code != codeIdempotencyKeyConflict {
		t.Fatalf("error = %#v, want code %q", body, codeIdempotencyKeyConflict)
	}
}

func TestCreateOrderUnexpectedFailureHidesCause(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(&stubOrders{err: errors.New("postgres: connection refused to 10.0.0.1")})
	recorder := postOrder(t, handler, `{"productId":"product_demo","quantity":1}`, map[string]string{"Idempotency-Key": "k"})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeError(t, recorder)
	if body.Code != codeInternalError || strings.Contains(body.Message, "postgres") {
		t.Fatalf("error = %#v, want a generic internal error", body)
	}
}

func TestCreateOrderNotServedWithoutOrders(t *testing.T) {
	t.Parallel()

	recorder := postOrder(t, newTestHandler(nil), `{"productId":"product_demo","quantity":1}`, map[string]string{"Idempotency-Key": "k"})

	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (%s)", recorder.Code, recorder.Body.String())
	}
	if body := decodeError(t, recorder); body.Code != codeNotImplemented {
		t.Fatalf("error = %#v, want code %q", body, codeNotImplemented)
	}
}
