package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func assertStrictSecurityHeaders(t *testing.T, header http.Header) {
	t.Helper()
	want := map[string]string{
		headerContentTypeOptions: "nosniff",
		headerReferrerPolicy:     "no-referrer",
		headerCacheControl:       "no-store",
		headerContentSecurity:    strictContentSecurityPolicy,
		headerFrameOptions:       "DENY",
	}
	for name, value := range want {
		if got := header.Get(name); got != value {
			t.Fatalf("%s = %q, want %q (headers: %v)", name, got, value, header)
		}
	}
	if header.Get(correlationHeader) == "" {
		t.Fatalf("%s is absent (headers: %v)", correlationHeader, header)
	}
	for name := range header {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Fatalf("CORS header %s is emitted without being configured", name)
		}
	}
}

// TestSecurityHeadersOnEveryOutcome covers success, binding errors, missing
// credentials, unknown paths, handler failures and the recovery path, which
// are the different code paths that write a response.
func TestSecurityHeadersOnEveryOutcome(t *testing.T) {
	t.Parallel()

	orderBody := `{"productId":"product_demo","quantity":1}`
	tests := []struct {
		name    string
		handler http.Handler
		request func() *http.Request
		status  int
	}{
		{
			name:    "200 success",
			handler: newTestHandler(realOrders(t)),
			request: func() *http.Request {
				return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
			},
			status: http.StatusOK,
		},
		{
			name:    "400 binding error",
			handler: newTestHandler(realOrders(t)),
			request: func() *http.Request {
				request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders", strings.NewReader(orderBody))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(apiKeyHeader, testAPIKey)
				return request
			},
			status: http.StatusBadRequest,
		},
		{
			name:    "401 unauthenticated",
			handler: newTestHandler(realOrders(t)),
			request: func() *http.Request {
				request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders", strings.NewReader(orderBody))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", "headers-401")
				return request
			},
			status: http.StatusUnauthorized,
		},
		{
			name:    "404 unknown path",
			handler: newTestHandler(realOrders(t)),
			request: func() *http.Request {
				return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/unknown", nil)
			},
			status: http.StatusNotFound,
		},
		{
			name:    "500 handler failure",
			handler: newTestHandler(&stubOrders{err: errors.New("postgres down")}),
			request: func() *http.Request {
				request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders", strings.NewReader(orderBody))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", "headers-500")
				request.Header.Set(apiKeyHeader, testAPIKey)
				return request
			},
			status: http.StatusInternalServerError,
		},
		{
			name:    "500 recovered panic",
			handler: newTestHandler(panicOrders{}),
			request: func() *http.Request {
				request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders", strings.NewReader(orderBody))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", "headers-panic")
				request.Header.Set(apiKeyHeader, testAPIKey)
				return request
			},
			status: http.StatusInternalServerError,
		},
		{
			name:    "405 method not allowed",
			handler: newTestHandler(realOrders(t)),
			request: func() *http.Request {
				request := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/v1/orders", nil)
				request.Header.Set("Origin", "https://evil.example")
				return request
			},
			status: http.StatusMethodNotAllowed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			test.handler.ServeHTTP(recorder, test.request())
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, test.status, recorder.Body.String())
			}
			assertStrictSecurityHeaders(t, recorder.Header())
		})
	}
}

// TestRecoveryRestoresStrictHeadersAfterRelaxedPolicy proves a handler that
// relaxed the policy for its own body and then failed does not leak the
// relaxed policy on the error it never meant to send.
func TestRecoveryRestoresStrictHeadersAfterRelaxedPolicy(t *testing.T) {
	t.Parallel()

	relaxedThenPanics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerContentSecurity, "default-src 'self' 'unsafe-inline'")
		w.Header().Set(headerCacheControl, "public, max-age=3600")
		panic("boom")
	})
	handler := correlationMiddleware(securityHeadersMiddleware(recoveryMiddleware(zap.NewNop(), relaxedThenPanics)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/anything", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	assertStrictSecurityHeaders(t, recorder.Header())
}
