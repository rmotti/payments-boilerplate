package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

type verifierFunc func(candidate string) bool

func (f verifierFunc) Valid(candidate string) bool { return f(candidate) }

func TestAPIKeyAuthenticationMiddlewareAllowsPublicOperation(t *testing.T) {
	t.Parallel()

	called := false
	next := func(context.Context, http.ResponseWriter, *http.Request, any) (any, error) {
		called = true
		return nil, nil
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	_, err := apiKeyAuthenticationMiddleware(nil)(next, "GetHealth")(
		request.Context(), httptest.NewRecorder(), request, openapi.GetHealthRequestObject{},
	)
	if err != nil || !called {
		t.Fatalf("public operation error/called = %v/%t, want nil/true", err, called)
	}
}

func TestAPIKeyAuthenticationMiddlewareProtectsOperationsByDefault(t *testing.T) {
	t.Parallel()

	for _, operationID := range []string{"CreateOrder", "CreateCheckout", "GetOrder", "FutureOperation"} {
		t.Run(operationID, func(t *testing.T) {
			t.Parallel()
			called := false
			next := func(context.Context, http.ResponseWriter, *http.Request, any) (any, error) {
				called = true
				return nil, nil
			}
			request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/protected-operation", nil)
			_, err := apiKeyAuthenticationMiddleware(nil)(next, operationID)(
				request.Context(), httptest.NewRecorder(), request, nil,
			)
			if !errors.Is(err, ErrUnauthorized) || called {
				t.Fatalf("protected operation error/called = %v/%t, want unauthorized/false", err, called)
			}
		})
	}
}

func TestAPIKeyAuthenticationMiddlewareAcceptsValidKey(t *testing.T) {
	t.Parallel()

	const key = "valid-key"
	called := false
	next := func(context.Context, http.ResponseWriter, *http.Request, any) (any, error) {
		called = true
		return nil, nil
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/orders", nil)
	request.Header.Set(apiKeyHeader, key)
	verifier := verifierFunc(func(candidate string) bool { return candidate == key })
	_, err := apiKeyAuthenticationMiddleware(verifier)(next, "CreateOrder")(
		request.Context(), httptest.NewRecorder(), request, nil,
	)
	if err != nil || !called {
		t.Fatalf("authenticated operation error/called = %v/%t, want nil/true", err, called)
	}
}
