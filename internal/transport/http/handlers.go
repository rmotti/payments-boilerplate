package httpserver

import (
	"context"
	"errors"
	"fmt"
	"io"

	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	orderdomain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	webhookdomain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

// ErrNotServed marks an operation the contract declares but this process does
// not run. It answers a route that exists and is not served; a route a process
// does not register at all is simply absent and answers 404.
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

// WebhookReceiver durably accepts one verified provider event.
type WebhookReceiver interface {
	Receive(ctx context.Context, in webhookapp.ReceiveInput) (webhookapp.Outcome, error)
}

// WebhookOperations exposes non-sensitive inspection and failed-event replay.
type WebhookOperations interface {
	List(ctx context.Context, status webhookdomain.Status, limit int) ([]webhookapp.EventInspection, error)
	Reprocess(ctx context.Context, eventID string) (webhookapp.EventInspection, error)
}

// APIHandler implements the generated strict OpenAPI contract.
type APIHandler struct {
	health     *health.Service
	orders     OrderCreator
	checkouts  CheckoutCreator
	webhooks   WebhookReceiver
	operations WebhookOperations
}

// NewAPIHandler composes the HTTP handlers required by the OpenAPI contract.
// A nil dependency makes its operations answer 501, for a process that serves
// part of the contract. A process that serves none of it, such as the worker,
// uses NewHealthOnly instead and never registers those routes at all.
func NewAPIHandler(
	healthService *health.Service,
	orders OrderCreator,
	checkouts CheckoutCreator,
	webhooks WebhookReceiver,
	operations ...WebhookOperations,
) *APIHandler {
	handler := &APIHandler{health: healthService, orders: orders, checkouts: checkouts, webhooks: webhooks}
	if len(operations) > 0 {
		handler.operations = operations[0]
	}
	return handler
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
		return openapi.GetHealth503JSONResponse{
			Body: response, Headers: openapi.GetHealth503ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	}

	return openapi.GetHealth200JSONResponse{
		Body: response, Headers: openapi.GetHealth200ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
	}, nil
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
		return openapi.CreateOrder400JSONResponse{
			Body:    newError(ctx, codeInvalidRequest, "request body is required"),
			Headers: openapi.CreateOrder400ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	}

	order, err := h.orders.Create(ctx, orderapp.CreateInput{
		ProductID:      request.Body.ProductId,
		Quantity:       request.Body.Quantity,
		IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		return createOrderError(ctx, err)
	}

	return openapi.CreateOrder201JSONResponse{
		Body:    orderResponse(order),
		Headers: openapi.CreateOrder201ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
	}, nil
}

func createOrderError(ctx context.Context, err error) (openapi.CreateOrderResponseObject, error) {
	switch {
	case errors.Is(err, orderdomain.ErrInvalidProductID),
		errors.Is(err, orderdomain.ErrInvalidQuantity),
		errors.Is(err, orderdomain.ErrInvalidIdempotencyKey):
		return openapi.CreateOrder400JSONResponse{
			Body:    newError(ctx, codeInvalidRequest, err.Error()),
			Headers: openapi.CreateOrder400ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	case errors.Is(err, orderapp.ErrProductNotFound):
		return openapi.CreateOrder404JSONResponse{
			Body:    newError(ctx, codeProductNotFound, orderapp.ErrProductNotFound.Error()),
			Headers: openapi.CreateOrder404ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	case errors.Is(err, orderapp.ErrIdempotencyKeyConflict):
		return openapi.CreateOrder409JSONResponse{
			Body:    newError(ctx, codeIdempotencyKeyConflict, orderapp.ErrIdempotencyKeyConflict.Error()),
			Headers: openapi.CreateOrder409ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
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
			return openapi.GetOrder404JSONResponse{
				Body:    newError(ctx, codeOrderNotFound, orderapp.ErrOrderNotFound.Error()),
				Headers: openapi.GetOrder404ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
			}, nil
		}
		return nil, fmt.Errorf("get order: %w", err)
	}
	return openapi.GetOrder200JSONResponse{
		Body:    orderResponse(order),
		Headers: openapi.GetOrder200ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
	}, nil
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
		Body:    openapi.Checkout{CheckoutUrl: checkout.URL, ExpiresAt: checkout.ExpiresAt},
		Headers: openapi.CreateCheckout201ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
	}, nil
}

func createCheckoutError(ctx context.Context, err error) (openapi.CreateCheckoutResponseObject, error) {
	switch {
	case errors.Is(err, orderdomain.ErrInvalidIdempotencyKey):
		return openapi.CreateCheckout400JSONResponse{
			Body:    newError(ctx, codeInvalidRequest, err.Error()),
			Headers: openapi.CreateCheckout400ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	case errors.Is(err, orderapp.ErrOrderNotFound):
		return openapi.CreateCheckout404JSONResponse{
			Body:    newError(ctx, codeOrderNotFound, orderapp.ErrOrderNotFound.Error()),
			Headers: openapi.CreateCheckout404ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	case errors.Is(err, paymentapp.ErrIdempotencyKeyConflict):
		return openapi.CreateCheckout409JSONResponse{
			Body:    newError(ctx, codeIdempotencyKeyConflict, paymentapp.ErrIdempotencyKeyConflict.Error()),
			Headers: openapi.CreateCheckout409ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	case errors.Is(err, paymentapp.ErrCheckoutInProgress):
		return openapi.CreateCheckout409JSONResponse{
			Body:    newError(ctx, codeCheckoutInProgress, paymentapp.ErrCheckoutInProgress.Error()),
			Headers: openapi.CreateCheckout409ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	case errors.Is(err, paymentapp.ErrOrderNotPayable):
		return openapi.CreateCheckout409JSONResponse{
			Body:    newError(ctx, codeOrderNotPayable, paymentapp.ErrOrderNotPayable.Error()),
			Headers: openapi.CreateCheckout409ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	case errors.Is(err, paymentapp.ErrProviderUnavailable):
		return openapi.CreateCheckout502JSONResponse{
			Body:    newError(ctx, codeProviderUnavailable, paymentapp.ErrProviderUnavailable.Error()),
			Headers: openapi.CreateCheckout502ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	default:
		return nil, fmt.Errorf("create checkout: %w", err)
	}
}

// ListWebhookEvents returns only operational metadata, never provider payloads.
func (h *APIHandler) ListWebhookEvents(
	ctx context.Context,
	request openapi.ListWebhookEventsRequestObject,
) (openapi.ListWebhookEventsResponseObject, error) {
	if h.operations == nil {
		return nil, ErrNotServed
	}
	status := webhookdomain.Status("")
	if request.Params.Status != nil {
		status = webhookdomain.Status(*request.Params.Status)
	}
	limit := 0
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	events, err := h.operations.List(ctx, status, limit)
	if err != nil {
		if errors.Is(err, webhookapp.ErrInvalidStatus) || errors.Is(err, webhookapp.ErrInvalidLimit) {
			return openapi.ListWebhookEvents400JSONResponse{
				Body:    newError(ctx, codeInvalidRequest, err.Error()),
				Headers: openapi.ListWebhookEvents400ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
			}, nil
		}
		return nil, fmt.Errorf("list webhook events: %w", err)
	}
	items := make([]openapi.WebhookEventInspection, 0, len(events))
	for _, event := range events {
		items = append(items, webhookEventResponse(event))
	}
	return openapi.ListWebhookEvents200JSONResponse{
		Body:    openapi.WebhookEventList{Items: items},
		Headers: openapi.ListWebhookEvents200ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
	}, nil
}

// ReprocessWebhookEvent atomically returns failed work to the outbox relay.
func (h *APIHandler) ReprocessWebhookEvent(
	ctx context.Context,
	request openapi.ReprocessWebhookEventRequestObject,
) (openapi.ReprocessWebhookEventResponseObject, error) {
	if h.operations == nil {
		return nil, ErrNotServed
	}
	event, err := h.operations.Reprocess(ctx, string(request.WebhookEventId))
	if err != nil {
		switch {
		case errors.Is(err, webhookapp.ErrEventNotFound):
			return openapi.ReprocessWebhookEvent404JSONResponse{
				Body:    newError(ctx, codeWebhookEventNotFound, webhookapp.ErrEventNotFound.Error()),
				Headers: openapi.ReprocessWebhookEvent404ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
			}, nil
		case errors.Is(err, webhookapp.ErrEventNotReplayable):
			return openapi.ReprocessWebhookEvent409JSONResponse{
				Body:    newError(ctx, codeWebhookEventNotReplayable, webhookapp.ErrEventNotReplayable.Error()),
				Headers: openapi.ReprocessWebhookEvent409ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
			}, nil
		default:
			return nil, fmt.Errorf("reprocess webhook event: %w", err)
		}
	}
	return openapi.ReprocessWebhookEvent202JSONResponse{
		Body:    webhookEventResponse(event),
		Headers: openapi.ReprocessWebhookEvent202ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
	}, nil
}

func webhookEventResponse(event webhookapp.EventInspection) openapi.WebhookEventInspection {
	response := openapi.WebhookEventInspection{
		Id: event.ID, Provider: string(event.Provider), ProviderEventId: event.ProviderEventID,
		EventType: event.EventType, Status: openapi.WebhookEventStatus(event.Status),
		Attempts: event.Attempts, ReceivedAt: event.ReceivedAt, ProcessedAt: event.ProcessedAt,
		LastError: optionalString(event.LastError), UpdatedAt: event.UpdatedAt,
		ReplayCount: event.ReplayCount, LastReplayedAt: event.LastReplayedAt,
	}
	if event.Outbox != nil {
		response.Outbox = &openapi.OutboxInspection{
			Id: event.Outbox.ID, Status: openapi.OutboxInspectionStatus(event.Outbox.Status),
			Attempts: event.Outbox.Attempts, PublishedAt: event.Outbox.PublishedAt,
			LastError: optionalString(event.Outbox.LastError), NextAttemptAt: event.Outbox.NextAttemptAt,
		}
	}
	return response
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// ReceiveStripeWebhook durably accepts a signed provider event.
//
// The status code decides whether the provider keeps the event alive: Stripe
// ends its retries only on 2xx and redelivers on anything else, 400 included.
// So the rule is to answer 2xx only once the event is durably stored, and to
// use the non-2xx codes to say why it was not. See ADR 0011.
func (h *APIHandler) ReceiveStripeWebhook(
	ctx context.Context,
	request openapi.ReceiveStripeWebhookRequestObject,
) (openapi.ReceiveStripeWebhookResponseObject, error) {
	if h.webhooks == nil {
		return nil, ErrNotServed
	}

	rawBody, readErr := readWebhookBody(request.Body)
	input := webhookapp.ReceiveInput{
		RawBody: rawBody, CorrelationID: correlationIDFromContext(ctx),
	}
	if request.Params.StripeSignature != nil {
		input.Signature = *request.Params.StripeSignature
	}
	if readErr != nil {
		// The body could not be read whole, so the signature could never be
		// checked. Unlike a forged signature this is our own limit, and a
		// redelivery works once it is raised, so the provider must retry. It is
		// still sent through the use case so the receive observer records the
		// failed request without attempting verification or persistence.
		input.RawBody = nil
		input.ReadError = fmt.Errorf("%w: %w", webhookapp.ErrPayloadTooLarge, readErr)
	}

	outcome, err := h.webhooks.Receive(ctx, input)
	if err != nil {
		if errors.Is(err, webhookapp.ErrInvalidSignature) {
			// Absent, forged and expired signatures answer the same way, so
			// the response never tells an attacker which of the three it was.
			// Stripe will still redeliver, and every attempt will fail the same
			// way until the endpoint secret is fixed; the invalid-signature
			// metric is what turns that into an alert.
			return openapi.ReceiveStripeWebhook400JSONResponse{
				Body:    newError(ctx, codeInvalidSignature, webhookapp.ErrInvalidSignature.Error()),
				Headers: openapi.ReceiveStripeWebhook400ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
			}, nil
		}
		// Storage failed. Answering 500 is deliberate: the provider is the only
		// thing that can deliver this event again.
		return nil, fmt.Errorf("receive stripe webhook: %w", err)
	}

	if outcome == webhookapp.OutcomeAccepted {
		return openapi.ReceiveStripeWebhook202Response{
			Headers: openapi.ReceiveStripeWebhook202ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
		}, nil
	}
	// Duplicate or ignored: nothing was queued, and nothing should be retried.
	return openapi.ReceiveStripeWebhook200Response{
		Headers: openapi.ReceiveStripeWebhook200ResponseHeaders{XCorrelationID: correlationIDFromContext(ctx)},
	}, nil
}

// readWebhookBody reads the request bytes exactly as they arrived. They must
// reach signature verification untouched: parsing and re-encoding would reorder
// keys and drop whitespace, and the recomputed signature would never match.
func readWebhookBody(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	return io.ReadAll(body)
}
