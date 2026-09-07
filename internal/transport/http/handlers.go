package httpserver

import (
	"context"
	"errors"
	"fmt"

	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	orderdomain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

// ErrNotServed marks an operation the contract declares but this process does
// not run, such as order creation on the worker's health-only listener.
var ErrNotServed = errors.New("operation not served by this process")

// OrderCreator is the slice of the order use cases the HTTP layer depends on.
type OrderCreator interface {
	Create(ctx context.Context, in orderapp.CreateInput) (orderdomain.Order, error)
	Get(ctx context.Context, id string) (orderdomain.Order, error)
}

// CheckoutCreator is the checkout capability required by the HTTP layer.
type CheckoutCreator interface {
	CreateCheckout(ctx context.Context, in paymentapp.CreateInput) (paymentapp.Checkout, error)
}

// APIHandler implements the generated strict OpenAPI contract.
type APIHandler struct {
	health    *health.Service
	orders    OrderCreator
	checkouts CheckoutCreator
}

// NewAPIHandler composes the HTTP handlers required by the OpenAPI contract.
// A nil orders service makes order operations answer 501, which is what the
// worker wants: it shares the contract but only serves health.
func NewAPIHandler(healthService *health.Service, orders OrderCreator, checkouts CheckoutCreator) *APIHandler {
	return &APIHandler{health: healthService, orders: orders, checkouts: checkouts}
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

	order, err := h.orders.Create(ctx, orderapp.CreateInput{
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
	case errors.Is(err, orderdomain.ErrInvalidProductID),
		errors.Is(err, orderdomain.ErrInvalidQuantity),
		errors.Is(err, orderdomain.ErrInvalidIdempotencyKey):
		return openapi.CreateOrder400JSONResponse(newError(ctx, codeInvalidRequest, err.Error())), nil
	case errors.Is(err, orderapp.ErrProductNotFound):
		return openapi.CreateOrder404JSONResponse(newError(ctx, codeProductNotFound, orderapp.ErrProductNotFound.Error())), nil
	case errors.Is(err, orderapp.ErrIdempotencyKeyConflict):
		return openapi.CreateOrder409JSONResponse(newError(ctx, codeIdempotencyKeyConflict, orderapp.ErrIdempotencyKeyConflict.Error())), nil
	default:
		// Anything else is unexpected. Hand it to the server so it is logged
		// with the correlation id and answered as a generic 500.
		return nil, fmt.Errorf("create order: %w", err)
	}
}

func orderResponse(order orderdomain.Order) openapi.Order {
	return openapi.Order{
		Id:       order.ID,
		Status:   openapi.OrderStatus(order.Status),
		Amount:   order.Amount,
		Currency: string(order.Currency),
	}
}

// GetOrder returns the state persisted locally; redirect URLs never change it.
func (h *APIHandler) GetOrder(
	ctx context.Context,
	request openapi.GetOrderRequestObject,
) (openapi.GetOrderResponseObject, error) {
	if h.orders == nil {
		return nil, ErrNotServed
	}
	order, err := h.orders.Get(ctx, string(request.OrderId))
	if err != nil {
		if errors.Is(err, orderapp.ErrOrderNotFound) {
			return openapi.GetOrder404JSONResponse(newError(ctx, codeOrderNotFound, orderapp.ErrOrderNotFound.Error())), nil
		}
		return nil, fmt.Errorf("get order: %w", err)
	}
	return openapi.GetOrder200JSONResponse(orderResponse(order)), nil
}

// CreateCheckout opens or recovers an idempotent Stripe hosted session.
func (h *APIHandler) CreateCheckout(
	ctx context.Context,
	request openapi.CreateCheckoutRequestObject,
) (openapi.CreateCheckoutResponseObject, error) {
	if h.checkouts == nil {
		return nil, ErrNotServed
	}
	checkout, err := h.checkouts.CreateCheckout(ctx, paymentapp.CreateInput{
		OrderID: string(request.OrderId), IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		return createCheckoutError(ctx, err)
	}
	return openapi.CreateCheckout201JSONResponse{
		CheckoutUrl: checkout.URL, ExpiresAt: checkout.ExpiresAt,
	}, nil
}

func createCheckoutError(ctx context.Context, err error) (openapi.CreateCheckoutResponseObject, error) {
	switch {
	case errors.Is(err, orderdomain.ErrInvalidIdempotencyKey):
		return openapi.CreateCheckout400JSONResponse(newError(ctx, codeInvalidRequest, err.Error())), nil
	case errors.Is(err, orderapp.ErrOrderNotFound):
		return openapi.CreateCheckout404JSONResponse(newError(ctx, codeOrderNotFound, orderapp.ErrOrderNotFound.Error())), nil
	case errors.Is(err, paymentapp.ErrIdempotencyKeyConflict):
		return openapi.CreateCheckout409JSONResponse(newError(ctx, codeIdempotencyKeyConflict, paymentapp.ErrIdempotencyKeyConflict.Error())), nil
	case errors.Is(err, paymentapp.ErrCheckoutInProgress):
		return openapi.CreateCheckout409JSONResponse(newError(ctx, codeCheckoutInProgress, paymentapp.ErrCheckoutInProgress.Error())), nil
	case errors.Is(err, paymentapp.ErrOrderNotPayable):
		return openapi.CreateCheckout409JSONResponse(newError(ctx, codeOrderNotPayable, paymentapp.ErrOrderNotPayable.Error())), nil
	case errors.Is(err, paymentapp.ErrProviderUnavailable):
		return openapi.CreateCheckout502JSONResponse(newError(ctx, codeProviderUnavailable, paymentapp.ErrProviderUnavailable.Error())), nil
	default:
		return nil, fmt.Errorf("create checkout: %w", err)
	}
}
