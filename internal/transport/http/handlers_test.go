package httpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

func TestGetHealthReady(t *testing.T) {
	t.Parallel()

	handler := NewAPIHandler(health.New("payments-api", "test", map[string]health.Checker{
		"postgres": func(context.Context) error { return nil },
	}))

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
	}))

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
