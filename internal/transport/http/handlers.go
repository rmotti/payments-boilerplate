package httpserver

import (
	"context"

	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

// APIHandler implements the generated strict OpenAPI contract.
type APIHandler struct {
	health *health.Service
}

// NewAPIHandler composes the HTTP handlers required by the OpenAPI contract.
func NewAPIHandler(healthService *health.Service) *APIHandler {
	return &APIHandler{health: healthService}
}

// GetHealth returns aggregate process readiness.
func (h *APIHandler) GetHealth(
	ctx context.Context,
	_ openapi.GetHealthRequestObject,
) (openapi.GetHealthResponseObject, error) {
	result := h.health.Check(ctx)
	checks := make(map[string]openapi.HealthResponseChecks, len(result.Checks))
	for name, available := range result.Checks {
		checks[name] = openapi.Down
		if available {
			checks[name] = openapi.Up
		}
	}

	response := openapi.HealthResponse{
		Checks:  checks,
		Service: h.health.ServiceName(),
		Status:  openapi.Ok,
		Version: h.health.Version(),
	}
	if !result.Ready {
		response.Status = openapi.Unavailable
		return openapi.GetHealth503JSONResponse(response), nil
	}

	return openapi.GetHealth200JSONResponse(response), nil
}
