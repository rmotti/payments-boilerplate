package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/contracttest"
	orderdomain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	webhookdomain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"go.uber.org/zap"
)

type contractCase struct {
	name        string
	operationID string
	status      int
	request     func() *http.Request
	handler     func() http.Handler
	requestKind contracttest.RequestExpectation
	assert      func(*testing.T, []byte)
}

func TestHandlerResponsesMatchOpenAPI(t *testing.T) {
	t.Parallel()

	validator := newContractValidator(t)
	testCases := handlerContractCases(t, contractJSONFixture(t, "create-order-request"))
	assertCasesMatchManifest(t, testCases, contractCoverageManifest())
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			recorder, err := validator.Exercise(
				context.Background(), testCase.handler(), testCase.request(),
				testCase.operationID, testCase.status, testCase.requestKind,
			)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.assert != nil {
				testCase.assert(t, recorder.Body.Bytes())
			}
		})
	}
}

func TestRecoveryAndBodyLimitResponsesMatchOpenAPI(t *testing.T) {
	t.Parallel()

	validator := newContractValidator(t)
	orderRequest := contractJSONFixture(t, "create-order-request")
	t.Run("recovery", func(t *testing.T) {
		t.Parallel()
		_, err := validator.Exercise(context.Background(), newTestHandler(panicOrders{}),
			newContractRequest(http.MethodPost, "/v1/orders", orderRequest, true, true),
			"createOrder", http.StatusInternalServerError, contracttest.RequestValid)
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("webhook endpoint body limit", func(t *testing.T) {
		t.Parallel()
		request := newContractRequest(http.MethodPost, webhookPath,
			strings.Repeat("x", webhookMaxBodyBytes+1), false, false)
		request.Header.Set("Content-Type", "application/octet-stream")
		_, err := validator.Exercise(context.Background(),
			newTestHandlerWith(nil, nil, &stubWebhooks{outcome: webhookapp.OutcomeAccepted}),
			request, "receiveStripeWebhook", http.StatusInternalServerError, contracttest.RequestValid)
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestOpenAPIExamplesAndMarkedMarkdownContracts(t *testing.T) {
	t.Parallel()

	validator := newContractValidator(t)
	// New validates every example/example collection while it validates the
	// document. Keep an explicit count so removal of all examples cannot make
	// this check vacuous.
	if count := countOpenAPIExamples(validator); count == 0 {
		t.Fatal("OpenAPI document has no examples to validate")
	}

	examples, err := contracttest.LoadMarkdownExamples(repositoryPath(t, "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 0 {
		t.Fatal("docs/api.md has no explicitly marked contractual JSON blocks")
	}
	seenNames := make(map[string]struct{}, len(examples))
	for _, example := range examples {
		if _, duplicate := seenNames[example.Name]; duplicate {
			t.Fatalf("duplicate Markdown contract name %q", example.Name)
		}
		seenNames[example.Name] = struct{}{}
		schema, err := markdownContractSchema(validator.Document, example)
		if err != nil {
			t.Fatalf("Markdown contract %q: %v", example.Name, err)
		}
		if err := schema.VisitJSON(example.Value); err != nil {
			t.Fatalf("Markdown contract %q does not match %s %s %s: %v",
				example.Name, example.OperationID, example.Direction, example.Status, err)
		}
	}
}

func TestContractCoverageManifestMatchesOpenAPI(t *testing.T) {
	t.Parallel()

	validator := newContractValidator(t)
	if err := contracttest.ValidateManifest(validator.Document, contractCoverageManifest()); err != nil {
		t.Fatal(err)
	}
}

func handlerContractCases(t *testing.T, orderRequestBody string) []contractCase {
	t.Helper()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	orderID := "ord_0123456789abcdef0123456789abcdef"
	eventID := "evt_0123456789abcdef0123456789abcdef"
	order := orderdomain.Order{ID: orderID, Status: orderdomain.StatusPending, Amount: 10000, Currency: orderdomain.BRL}
	checkout := paymentapp.Checkout{URL: "https://checkout.stripe.com/c/pay/test", ExpiresAt: now.Add(time.Hour)}
	event := webhookapp.EventInspection{
		ID: eventID, Provider: webhookdomain.Stripe, ProviderEventID: "evt_provider",
		EventType: "checkout.session.completed", Status: webhookdomain.StatusFailed,
		Attempts: 1, ReceivedAt: now, UpdatedAt: now, ReplayCount: 0,
	}

	createOrderRequest := func(authenticated, idempotent bool) func() *http.Request {
		return func() *http.Request {
			return newContractRequest(http.MethodPost, "/v1/orders", orderRequestBody, authenticated, idempotent)
		}
	}
	getOrderRequest := func(authenticated bool) func() *http.Request {
		return func() *http.Request {
			return newContractRequest(http.MethodGet, "/v1/orders/"+orderID, "", authenticated, false)
		}
	}
	checkoutRequest := func(authenticated, idempotent bool) func() *http.Request {
		return func() *http.Request {
			return newContractRequest(http.MethodPost, "/v1/orders/"+orderID+"/checkout", "", authenticated, idempotent)
		}
	}
	listRequest := func(authenticated bool) func() *http.Request {
		return func() *http.Request {
			return newContractRequest(http.MethodGet, "/v1/webhook-events", "", authenticated, false)
		}
	}
	reprocessRequest := func(authenticated bool) func() *http.Request {
		return func() *http.Request {
			return newContractRequest(http.MethodPost, "/v1/webhook-events/"+eventID+"/reprocess", "", authenticated, false)
		}
	}
	webhookRequest := func() *http.Request {
		request := newContractRequest(http.MethodPost, webhookPath, `{"id":"evt_provider"}`, false, false)
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("Stripe-Signature", "t=1,v1=abc")
		return request
	}
	operations := func(item webhookapp.EventInspection, err error) func() http.Handler {
		return func() http.Handler {
			items := []webhookapp.EventInspection{item}
			return newTestHandlerWithOperations(nil, nil, nil, &webhookOperationsStub{items: items, err: err})
		}
	}

	return []contractCase{
		{name: "getHealth/200", operationID: "getHealth", status: 200, request: healthRequest,
			handler: func() http.Handler { return newContractHealthHandler(nil) }, requestKind: contracttest.RequestValid},
		{name: "getHealth/503", operationID: "getHealth", status: 503, request: healthRequest,
			handler: func() http.Handler { return newContractHealthHandler(errors.New("postgres unavailable")) }, requestKind: contracttest.RequestValid},

		{name: "createOrder/201", operationID: "createOrder", status: 201, request: createOrderRequest(true, true),
			handler: func() http.Handler { return newTestHandler(&stubOrders{order: order}) }, requestKind: contracttest.RequestValid},
		{name: "createOrder/400", operationID: "createOrder", status: 400, request: createOrderRequest(true, false),
			handler: func() http.Handler { return newTestHandler(&stubOrders{order: order}) }, requestKind: contracttest.RequestInvalid},
		{name: "createOrder/401", operationID: "createOrder", status: 401, request: createOrderRequest(false, true),
			handler: func() http.Handler { return newTestHandler(&stubOrders{order: order}) }, requestKind: contracttest.RequestValid},
		{name: "createOrder/404", operationID: "createOrder", status: 404, request: createOrderRequest(true, true),
			handler: func() http.Handler { return newTestHandler(&stubOrders{err: orderapp.ErrProductNotFound}) }, requestKind: contracttest.RequestValid},
		{name: "createOrder/409", operationID: "createOrder", status: 409, request: createOrderRequest(true, true),
			handler: func() http.Handler { return newTestHandler(&stubOrders{err: orderapp.ErrIdempotencyKeyConflict}) }, requestKind: contracttest.RequestValid},
		{name: "createOrder/413", operationID: "createOrder", status: 413,
			request: func() *http.Request {
				body := `{"productId":"` + strings.Repeat("a", maxBodyBytes) + `","quantity":1}`
				return newContractRequest(http.MethodPost, "/v1/orders", body, true, true)
			}, handler: func() http.Handler { return newTestHandler(&stubOrders{order: order}) }, requestKind: contracttest.RequestInvalid},
		{name: "createOrder/500", operationID: "createOrder", status: 500, request: createOrderRequest(true, true),
			handler: func() http.Handler { return newTestHandler(&stubOrders{err: errors.New("database unavailable")}) }, requestKind: contracttest.RequestValid},

		{name: "getOrder/200", operationID: "getOrder", status: 200, request: getOrderRequest(true),
			handler: func() http.Handler { return newTestHandler(&stubOrders{order: order}) }, requestKind: contracttest.RequestValid},
		{name: "getOrder/401", operationID: "getOrder", status: 401, request: getOrderRequest(false),
			handler: func() http.Handler { return newTestHandler(&stubOrders{order: order}) }, requestKind: contracttest.RequestValid},
		{name: "getOrder/404", operationID: "getOrder", status: 404, request: getOrderRequest(true),
			handler: func() http.Handler { return newTestHandler(&stubOrders{err: orderapp.ErrOrderNotFound}) }, requestKind: contracttest.RequestValid},
		{name: "getOrder/500", operationID: "getOrder", status: 500, request: getOrderRequest(true),
			handler: func() http.Handler { return newTestHandler(&stubOrders{err: errors.New("database unavailable")}) }, requestKind: contracttest.RequestValid},

		{name: "createCheckout/201", operationID: "createCheckout", status: 201, request: checkoutRequest(true, true),
			handler: func() http.Handler { return newTestHandlerWithCheckout(nil, &stubCheckouts{checkout: checkout}) }, requestKind: contracttest.RequestValid},
		{name: "createCheckout/400", operationID: "createCheckout", status: 400, request: checkoutRequest(true, false),
			handler: func() http.Handler { return newTestHandlerWithCheckout(nil, &stubCheckouts{checkout: checkout}) }, requestKind: contracttest.RequestInvalid},
		{name: "createCheckout/401", operationID: "createCheckout", status: 401, request: checkoutRequest(false, true),
			handler: func() http.Handler { return newTestHandlerWithCheckout(nil, &stubCheckouts{checkout: checkout}) }, requestKind: contracttest.RequestValid},
		{name: "createCheckout/404", operationID: "createCheckout", status: 404, request: checkoutRequest(true, true),
			handler: func() http.Handler {
				return newTestHandlerWithCheckout(nil, &stubCheckouts{err: orderapp.ErrOrderNotFound})
			}, requestKind: contracttest.RequestValid},
		{name: "createCheckout/409", operationID: "createCheckout", status: 409, request: checkoutRequest(true, true),
			handler: func() http.Handler {
				return newTestHandlerWithCheckout(nil, &stubCheckouts{err: paymentapp.ErrCheckoutInProgress})
			}, requestKind: contracttest.RequestValid},
		{name: "createCheckout/502", operationID: "createCheckout", status: 502, request: checkoutRequest(true, true),
			handler: func() http.Handler {
				return newTestHandlerWithCheckout(nil, &stubCheckouts{err: paymentapp.ErrProviderUnavailable})
			}, requestKind: contracttest.RequestValid},
		{name: "createCheckout/500", operationID: "createCheckout", status: 500, request: checkoutRequest(true, true),
			handler: func() http.Handler {
				return newTestHandlerWithCheckout(nil, &stubCheckouts{err: errors.New("database unavailable")})
			}, requestKind: contracttest.RequestValid},

		{name: "listWebhookEvents/200", operationID: "listWebhookEvents", status: 200, request: listRequest(true),
			handler: operations(event, nil), requestKind: contracttest.RequestValid, assert: assertNoProviderPayload},
		{name: "listWebhookEvents/400", operationID: "listWebhookEvents", status: 400, request: listRequest(true),
			handler: operations(event, webhookapp.ErrInvalidLimit), requestKind: contracttest.RequestValid},
		{name: "listWebhookEvents/401", operationID: "listWebhookEvents", status: 401, request: listRequest(false),
			handler: operations(event, nil), requestKind: contracttest.RequestValid},
		{name: "listWebhookEvents/500", operationID: "listWebhookEvents", status: 500, request: listRequest(true),
			handler: operations(event, errors.New("database unavailable")), requestKind: contracttest.RequestValid},

		{name: "reprocessWebhookEvent/202", operationID: "reprocessWebhookEvent", status: 202, request: reprocessRequest(true),
			handler: operations(event, nil), requestKind: contracttest.RequestValid},
		{name: "reprocessWebhookEvent/401", operationID: "reprocessWebhookEvent", status: 401, request: reprocessRequest(false),
			handler: operations(event, nil), requestKind: contracttest.RequestValid},
		{name: "reprocessWebhookEvent/404", operationID: "reprocessWebhookEvent", status: 404, request: reprocessRequest(true),
			handler: operations(event, webhookapp.ErrEventNotFound), requestKind: contracttest.RequestValid},
		{name: "reprocessWebhookEvent/409", operationID: "reprocessWebhookEvent", status: 409, request: reprocessRequest(true),
			handler: operations(event, webhookapp.ErrEventNotReplayable), requestKind: contracttest.RequestValid},
		{name: "reprocessWebhookEvent/500", operationID: "reprocessWebhookEvent", status: 500, request: reprocessRequest(true),
			handler: operations(event, errors.New("database unavailable")), requestKind: contracttest.RequestValid},

		{name: "receiveStripeWebhook/202", operationID: "receiveStripeWebhook", status: 202, request: webhookRequest,
			handler: func() http.Handler {
				return newTestHandlerWith(nil, nil, &stubWebhooks{outcome: webhookapp.OutcomeAccepted})
			}, requestKind: contracttest.RequestValid},
		{name: "receiveStripeWebhook/200", operationID: "receiveStripeWebhook", status: 200, request: webhookRequest,
			handler: func() http.Handler {
				return newTestHandlerWith(nil, nil, &stubWebhooks{outcome: webhookapp.OutcomeDuplicate})
			}, requestKind: contracttest.RequestValid},
		{name: "receiveStripeWebhook/400", operationID: "receiveStripeWebhook", status: 400, request: webhookRequest,
			handler: func() http.Handler {
				return newTestHandlerWith(nil, nil, &stubWebhooks{err: webhookapp.ErrInvalidSignature})
			}, requestKind: contracttest.RequestValid},
		{name: "receiveStripeWebhook/500", operationID: "receiveStripeWebhook", status: 500, request: webhookRequest,
			handler: func() http.Handler {
				return newTestHandlerWith(nil, nil, &stubWebhooks{err: errors.New("database unavailable")})
			}, requestKind: contracttest.RequestValid},
	}
}

func newContractRequest(method, path, body string, authenticated, idempotent bool) *http.Request {
	request, err := http.NewRequestWithContext(context.Background(), method, "http://contract.test"+path, strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		request.Header.Set(apiKeyHeader, testAPIKey)
	}
	if idempotent {
		request.Header.Set("Idempotency-Key", "contract-key")
	}
	return request
}

func healthRequest() *http.Request {
	return newContractRequest(http.MethodGet, "/health", "", false, false)
}

func newContractHealthHandler(checkErr error) http.Handler {
	service := health.New("payments-api", "test", map[string]health.Checker{
		"postgres": func(context.Context) error { return checkErr },
	})
	api := NewAPIHandler(service, nil, nil, nil)
	return New(Config{Address: ":0", ShutdownTimeout: time.Second}, zap.NewNop(), api, nil).server.Handler
}

type panicOrders struct{}

func (panicOrders) Create(context.Context, orderapp.CreateInput) (orderdomain.Order, error) {
	panic("contract recovery probe")
}

func (panicOrders) Get(context.Context, string) (orderdomain.Order, error) {
	panic("contract recovery probe")
}

func assertNoProviderPayload(t *testing.T, body []byte) {
	t.Helper()
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode operational response: %v", err)
	}
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
				if normalized == "payload" || normalized == "rawpayload" {
					t.Errorf("operational response leaks forbidden field %q", key)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
}

func newContractValidator(t *testing.T) *contracttest.Validator {
	t.Helper()
	validator, err := contracttest.New(context.Background(), repositoryPath(t, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func contractJSONFixture(t *testing.T, name string) string {
	t.Helper()
	examples, err := contracttest.LoadMarkdownExamples(repositoryPath(t, "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range examples {
		if example.Name != name {
			continue
		}
		value, err := json.Marshal(example.Value)
		if err != nil {
			t.Fatalf("encode Markdown contract %q: %v", name, err)
		}
		return string(value)
	}
	t.Fatalf("Markdown contract %q not found", name)
	return ""
}

func repositoryPath(t *testing.T, parts ...string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve contract test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
	return filepath.Join(append([]string{root}, parts...)...)
}

func countOpenAPIExamples(validator *contracttest.Validator) int {
	count := 0
	for _, pathItem := range validator.Document.Paths.Map() {
		for _, parameter := range pathItem.Parameters {
			count += countParameterExamples(parameter.Value)
		}
		for _, operation := range pathItem.Operations() {
			for _, parameter := range operation.Parameters {
				count += countParameterExamples(parameter.Value)
			}
			if operation.RequestBody != nil && operation.RequestBody.Value != nil {
				for _, mediaType := range operation.RequestBody.Value.Content {
					if mediaType.Example != nil {
						count++
					}
					count += len(mediaType.Examples)
				}
			}
			for _, response := range operation.Responses.Map() {
				if response.Value == nil {
					continue
				}
				for _, mediaType := range response.Value.Content {
					if mediaType.Example != nil {
						count++
					}
					count += len(mediaType.Examples)
				}
			}
		}
	}
	return count
}

func countParameterExamples(parameter *openapi3.Parameter) int {
	if parameter == nil {
		return 0
	}
	count := len(parameter.Examples)
	if parameter.Example != nil {
		count++
	}
	return count
}

func markdownContractSchema(document *openapi3.T, example contracttest.MarkdownExample) (*openapi3.Schema, error) {
	var operation *openapi3.Operation
	for _, pathItem := range document.Paths.Map() {
		for _, candidate := range pathItem.Operations() {
			if candidate.OperationID == example.OperationID {
				if operation != nil {
					return nil, fmt.Errorf("operationId %q is duplicated", example.OperationID)
				}
				operation = candidate
			}
		}
	}
	if operation == nil {
		return nil, fmt.Errorf("operationId %q is not declared", example.OperationID)
	}

	var content openapi3.Content
	if example.Direction == "request" {
		if operation.RequestBody == nil || operation.RequestBody.Value == nil {
			return nil, fmt.Errorf("operation has no request body")
		}
		content = operation.RequestBody.Value.Content
	} else {
		status, err := strconv.Atoi(example.Status)
		if err != nil {
			return nil, fmt.Errorf("response status %q is invalid", example.Status)
		}
		response := operation.Responses.Status(status)
		if response == nil || response.Value == nil {
			return nil, fmt.Errorf("response status %s is not declared", example.Status)
		}
		content = response.Value.Content
	}
	mediaType := content.Get("application/json")
	if mediaType == nil || mediaType.Schema == nil || mediaType.Schema.Value == nil {
		return nil, fmt.Errorf("contract point has no application/json schema")
	}
	return mediaType.Schema.Value, nil
}

func contractCoverageManifest() []contracttest.CoverageEntry {
	statuses := map[string][]int{
		"getHealth":             {200, 503},
		"createOrder":           {201, 400, 401, 404, 409, 413, 500},
		"getOrder":              {200, 401, 404, 500},
		"createCheckout":        {201, 400, 401, 404, 409, 502, 500},
		"listWebhookEvents":     {200, 400, 401, 500},
		"reprocessWebhookEvent": {202, 401, 404, 409, 500},
		"receiveStripeWebhook":  {200, 202, 400, 500},
	}
	entries := make([]contracttest.CoverageEntry, 0, 33)
	for operationID, operationStatuses := range statuses {
		for _, status := range operationStatuses {
			entries = append(entries, contracttest.CoverageEntry{
				OperationID: operationID,
				Status:      status,
				Layer:       "handler",
				CoveredBy:   fmt.Sprintf("TestHandlerResponsesMatchOpenAPI/%s/%d", operationID, status),
			})
		}
	}
	return entries
}

func assertCasesMatchManifest(
	t *testing.T,
	testCases []contractCase,
	manifest []contracttest.CoverageEntry,
) {
	t.Helper()
	cases := make(map[string]int, len(testCases))
	for _, testCase := range testCases {
		key := fmt.Sprintf("%s/%d", testCase.operationID, testCase.status)
		cases[key]++
	}
	for _, entry := range manifest {
		key := fmt.Sprintf("%s/%d", entry.OperationID, entry.Status)
		if cases[key] != 1 {
			t.Fatalf("manifest entry %s has %d handler contract cases, want exactly one", key, cases[key])
		}
		delete(cases, key)
	}
	if len(cases) != 0 {
		t.Fatalf("handler contract cases absent from manifest: %v", cases)
	}
}
