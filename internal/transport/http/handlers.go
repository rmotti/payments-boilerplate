package httpserver

import (
	"context"
	"errors"
	"fmt"

	app "github.com/rmotti/payments-boilerplate/internal/application/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

// ErrNotServed marks an operation the contract declares but this process does
// not run, such as order creation on the worker's health-only listener.
var ErrNotServed = errors.New("operation not served by this process")

// OrderCreator is the slice of the order use cases the HTTP layer depends on.
type OrderCreator interface {
	Create(ctx context.Context, in app.CreateInput) (domain.Order, error)
}

// APIHandler implements the generated strict OpenAPI contract.
type APIHandler struct {
	health *health.Service
	orders OrderCreator
}

// NewAPIHandler composes the HTTP handlers required by the OpenAPI contract.
// A nil orders service makes order operations answer 501, which is what the
// worker wants: it shares the contract but only serves health.
func NewAPIHandler(healthService *health.Service, orders OrderCreator) *APIHandler {
	return &APIHandler{health: healthService, orders: orders}
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

// CreateOrder prices and persists a pending order. The request never carries
// an amount; the response reports what the server computed.
func (h *APIHandler) CreateOrder(
	ctx context.Context,
	request openapi.CreateOrderRequestObject,
) (openapi.CreateOrderResponseObject, error) {
	if h.orders == nil {
		return nil, ErrNotServed
	}
	if request.Body == nil {
		return openapi.CreateOrder400JSONResponse(newError(ctx, codeInvalidRequest, "request body is required")), nil
	}

	order, err := h.orders.Create(ctx, app.CreateInput{
		ProductID:      request.Body.ProductId,
		Quantity:       request.Body.Quantity,
		IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		return createOrderError(ctx, err)
	}

	return openapi.CreateOrder201JSONResponse(orderResponse(order)), nil
}

func createOrderError(ctx context.Context, err error) (openapi.CreateOrderResponseObject, error) {
	switch {
	case errors.Is(err, domain.ErrInvalidProductID),
		errors.Is(err, domain.ErrInvalidQuantity),
		errors.Is(err, domain.ErrInvalidIdempotencyKey):
		return openapi.CreateOrder400JSONResponse(newError(ctx, codeInvalidRequest, err.Error())), nil
	case errors.Is(err, app.ErrProductNotFound):
		return openapi.CreateOrder404JSONResponse(newError(ctx, codeProductNotFound, app.ErrProductNotFound.Error())), nil
	case errors.Is(err, app.ErrIdempotencyKeyConflict):
		return openapi.CreateOrder409JSONResponse(newError(ctx, codeIdempotencyKeyConflict, app.ErrIdempotencyKeyConflict.Error())), nil
	default:
		// Anything else is unexpected. Hand it to the server so it is logged
		// with the correlation id and answered as a generic 500.
		return nil, fmt.Errorf("create order: %w", err)
	}
}

func orderResponse(order domain.Order) openapi.Order {
	return openapi.Order{
		Id:       order.ID,
		Status:   openapi.OrderStatus(order.Status),
		Amount:   order.Amount,
		Currency: string(order.Currency),
	}
}
