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
	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
	"go.uber.org/zap"
)

func TestGetHealthReady(t *testing.T) {
	t.Parallel()

	handler := NewAPIHandler(health.New("payments-api", "test", map[string]health.Checker{
		"postgres": func(context.Context) error { return nil },
	}), nil)

	response, err := handler.GetHealth(context.Background(), openapi.GetHealthRequestObject{})
	if err != nil {
		t.Fatalf("GetHealth() error = %v", err)
	}
	ready, ok := response.(openapi.GetHealth200JSONResponse)
	if !ok {
		t.Fatalf("GetHealth() response type = %T, want 200 response", response)
	}
	if ready.Status != openapi.Ok || ready.Checks["postgres"] != openapi.Up {
		t.Fatalf("GetHealth() response = %#v, want ready", ready)
	}
}

func TestGetHealthUnavailable(t *testing.T) {
	t.Parallel()

	handler := NewAPIHandler(health.New("payments-worker", "test", map[string]health.Checker{
		"rabbitmq": func(context.Context) error { return errors.New("unavailable") },
	}), nil)

	response, err := handler.GetHealth(context.Background(), openapi.GetHealthRequestObject{})
	if err != nil {
		t.Fatalf("GetHealth() error = %v", err)
	}
	unavailable, ok := response.(openapi.GetHealth503JSONResponse)
	if !ok {
		t.Fatalf("GetHealth() response type = %T, want 503 response", response)
	}
	if unavailable.Status != openapi.Unavailable || unavailable.Checks["rabbitmq"] != openapi.Down {
		t.Fatalf("GetHealth() response = %#v, want unavailable", unavailable)
	}
}

// stubOrders answers Create with a canned order or error and records the
// input it received.
type stubOrders struct {
	order domain.Order
	err   error
	got   app.CreateInput
}

func (s *stubOrders) Create(_ context.Context, in app.CreateInput) (domain.Order, error) {
	s.got = in
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
	return app.NewService(catalog, repoFunc(func(context.Context, domain.Order) error { return nil }),
		app.WithClock(func() time.Time { return time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC) }),
		app.WithIDGenerator(func() (string, error) { return "ord_fixed", nil }),
	)
}

type catalogFunc func(ctx context.Context, id string) (domain.Product, error)

func (f catalogFunc) Product(ctx context.Context, id string) (domain.Product, error) {
	return f(ctx, id)
}

type repoFunc func(ctx context.Context, order domain.Order) error

func (f repoFunc) Create(ctx context.Context, order domain.Order) error { return f(ctx, order) }

func newTestHandler(orders OrderCreator) http.Handler {
	api := NewAPIHandler(health.New("payments-api", "test", nil), orders)
	return New(Config{Address: ":0", ShutdownTimeout: time.Second}, zap.NewNop(), api).server.Handler
}

func postOrder(t *testing.T, handler http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
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
	want := openapi.Order{Id: "ord_fixed", Status: openapi.Pending, Amount: 30000, Currency: "BRL"}
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
