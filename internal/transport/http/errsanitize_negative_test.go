package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestUnexpectedFailureNeverLeaksSecretToLogsOrResponse is a negative test
// with a unique sentinel: an unexpected use case failure that carries a
// PostgreSQL DSN and an X-API-Key value in err.Error() must not surface that
// sentinel in the internal log (captured via an observer core) nor in the
// public HTTP response body. Only the stable [REDACTED] marker and the
// operationally useful "internal error" message may appear.
//
// This exercises the E8b requirement that logs, not only last_error and
// public responses, are sanitized: responseErrorHandler is the single place
// an unexpected use case error reaches both a log line and a client.
func TestUnexpectedFailureNeverLeaksSecretToLogsOrResponse(t *testing.T) {
	t.Parallel()

	const sentinel = "sentinel-http-log-4C7A1E"
	cause := errors.New(
		"dial postgres://payments:" + sentinel + "@db.internal:5432/payments failed; X-API-Key: " + sentinel,
	)

	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	orders := &stubOrders{err: cause}
	api := NewAPIHandler(health.New("payments-api", "test", nil), orders, nil, nil, nil)
	verifier := verifierFunc(func(candidate string) bool { return candidate == testAPIKey })
	handler := New(Config{Address: ":0", ShutdownTimeout: time.Second}, logger, api, verifier).server.Handler

	recorder := postOrder(t, handler, `{"productId":"product_demo","quantity":1}`, map[string]string{"Idempotency-Key": "k"})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), sentinel) {
		t.Fatalf("response body = %s, leaked the sentinel credential", recorder.Body.String())
	}
	body := decodeError(t, recorder)
	if body.Code != codeInternalError || !strings.Contains(body.Message, "internal error") {
		t.Fatalf("error = %#v, want a generic internal error", body)
	}
	if body.CorrelationId == "" {
		t.Fatal("response carried no correlation id")
	}

	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, sentinel) {
			t.Fatalf("log message = %q, leaked the sentinel credential", entry.Message)
		}
		for _, field := range entry.Context {
			rendered := field.String
			if strings.Contains(rendered, sentinel) {
				t.Fatalf("log field %q = %q, leaked the sentinel credential", field.Key, rendered)
			}
		}
	}
	if logs.FilterMessage("http handler failed").Len() != 1 {
		t.Fatalf("expected exactly one 'http handler failed' log entry, logs = %#v", logs.All())
	}
	errorField := logs.FilterMessage("http handler failed").All()[0].ContextMap()["error"]
	if !strings.Contains(errorField.(string), errsanitize.Redacted) {
		t.Fatalf("logged error field = %v, want the stable redaction marker", errorField)
	}
}

// TestHTTPSpansNeverCarryTheFailureSentinel is a negative test with a unique
// sentinel covering OpenTelemetry span data. The server wraps every request
// in otelhttp.NewHandler without any custom span attributes or events, so an
// unexpected failure carrying a credential must not reach a span through
// this package's own code. The tracer provider is swapped globally for the
// duration of the test and restored after, since otelhttp resolves it from
// otel.GetTracerProvider() at request time when none was configured on the
// handler.
func TestHTTPSpansNeverCarryTheFailureSentinel(t *testing.T) {
	// Not t.Parallel(): it mutates the global tracer provider.
	exporter := tracetest.NewInMemoryExporter()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	const sentinel = "sentinel-trace-6D1B8A"
	cause := errors.New(
		"dial postgres://payments:" + sentinel + "@db.internal:5432/payments failed",
	)

	orders := &stubOrders{err: cause}
	api := NewAPIHandler(health.New("payments-api", "test", nil), orders, nil, nil, nil)
	verifier := verifierFunc(func(candidate string) bool { return candidate == testAPIKey })
	handler := New(Config{Address: ":0", ShutdownTimeout: time.Second}, zap.NewNop(), api, verifier).server.Handler

	recorder := postOrder(t, handler, `{"productId":"product_demo","quantity":1}`, map[string]string{"Idempotency-Key": "k"})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", recorder.Code, recorder.Body.String())
	}
	if err := tracerProvider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush spans: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans were exported; the otelhttp instrumentation is expected to record one")
	}
	for _, span := range spans {
		if strings.Contains(span.Name, sentinel) {
			t.Fatalf("span name = %q, leaked the sentinel credential", span.Name)
		}
		for _, attr := range span.Attributes {
			if strings.Contains(attr.Value.String(), sentinel) {
				t.Fatalf("span attribute %q = %q, leaked the sentinel credential", attr.Key, attr.Value.String())
			}
		}
		for _, event := range span.Events {
			if strings.Contains(event.Name, sentinel) {
				t.Fatalf("span event name = %q, leaked the sentinel credential", event.Name)
			}
			for _, attr := range event.Attributes {
				if strings.Contains(attr.Value.String(), sentinel) {
					t.Fatalf("span event attribute %q = %q, leaked the sentinel credential", attr.Key, attr.Value.String())
				}
			}
		}
	}
}

// TestCheckoutURLNeverAppearsInAccessLogs is a negative test with a unique
// sentinel embedded in a checkout URL, the confidential value described in
// docs/security.md ("a URL de Checkout pode permitir retomar uma sessão
// ainda válida"). accessLogMiddleware only ever logs method, path, status
// and duration, never the response body, but this pins that invariant so a
// future change to the access log cannot start echoing response content
// without a test noticing.
func TestCheckoutURLNeverAppearsInAccessLogs(t *testing.T) {
	t.Parallel()

	const sentinel = "sentinel-checkout-url-2A8E5D"
	checkoutURL := "https://checkout.stripe.com/c/pay/cs_test_" + sentinel

	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	checkouts := &stubCheckouts{checkout: paymentapp.Checkout{URL: checkoutURL, ExpiresAt: time.Now()}}
	api := NewAPIHandler(health.New("payments-api", "test", nil), nil, checkouts, nil, nil)
	verifier := verifierFunc(func(candidate string) bool { return candidate == testAPIKey })
	handler := New(Config{Address: ":0", ShutdownTimeout: time.Second}, logger, api, verifier).server.Handler

	orderID := "ord_0123456789abcdef0123456789abcdef"
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders/"+orderID+"/checkout", nil)
	request.Header.Set(apiKeyHeader, testAPIKey)
	request.Header.Set("Idempotency-Key", "checkout-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), checkoutURL) {
		t.Fatalf("response body = %s, want the checkout URL: it is the endpoint's purpose", recorder.Body.String())
	}
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, sentinel) {
			t.Fatalf("log message = %q, leaked the checkout URL sentinel", entry.Message)
		}
		for _, field := range entry.Context {
			if strings.Contains(field.String, sentinel) {
				t.Fatalf("log field %q = %q, leaked the checkout URL sentinel", field.Key, field.String)
			}
		}
	}
}

// TestRecoveryMiddlewareNeverLeaksPanicValueToResponseOrLog confirms the
// panic path never surfaces the recovered value verbatim, in either the
// generic response body or the internal log: a panic built from a formatted
// driver or credential error is exactly as sensitive as one reached through
// a normal return.
func TestRecoveryMiddlewareNeverLeaksPanicValueToResponseOrLog(t *testing.T) {
	t.Parallel()

	const sentinel = "sentinel-panic-9B3D2F"
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("stripe secret sk_live_" + sentinel)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/whatever", nil)
	recoveryMiddleware(logger, panicking).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), sentinel) {
		t.Fatalf("response body = %s, leaked the panic sentinel", recorder.Body.String())
	}
	for _, entry := range logs.All() {
		for _, field := range entry.Context {
			if strings.Contains(field.String, sentinel) {
				t.Fatalf("log field %q = %q, leaked the panic sentinel", field.Key, field.String)
			}
		}
	}
	panicField := logs.FilterMessage("http handler panic").All()[0].ContextMap()["panic"]
	if !strings.Contains(panicField.(string), errsanitize.Redacted) {
		t.Fatalf("logged panic field = %v, want the stable redaction marker", panicField)
	}
}
