// Package contracttest provides reusable OpenAPI assertions for handler,
// integration and end-to-end tests.
package contracttest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
)

// RequestExpectation says whether a contract exercise intentionally sends a
// structurally valid or invalid request. Authentication is checked by the
// application, not by this structural validator.
type RequestExpectation bool

const (
	// RequestInvalid expects OpenAPI request validation to fail before the
	// real handler response is checked.
	RequestInvalid RequestExpectation = false
	// RequestValid expects the captured request to satisfy the OpenAPI schema.
	RequestValid RequestExpectation = true
)

// Validator owns one parsed copy of the real versioned OpenAPI document.
type Validator struct {
	Document *openapi3.T
	router   routers.Router
	options  *openapi3filter.Options
}

// New loads and validates the OpenAPI document, including every example that
// kin-openapi finds while traversing the document.
func New(ctx context.Context, specPath string) (*Validator, error) {
	document, err := openapi3.NewLoader().LoadFromFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI document: %w", err)
	}
	if err := document.Validate(ctx); err != nil {
		return nil, fmt.Errorf("validate OpenAPI document: %w", err)
	}
	// A relative root server is semantically equivalent to no base-path
	// restriction, but legacy.Router treats "/" as an exact URL prefix. Keep
	// the exposed real document intact and remove only that routing ambiguity
	// from a shallow copy.
	routingDocument := *document
	routingDocument.Servers = nil
	router, err := legacy.NewRouter(&routingDocument)
	if err != nil {
		return nil, fmt.Errorf("build OpenAPI router: %w", err)
	}
	return &Validator{
		Document: document,
		router:   router,
		options: &openapi3filter.Options{
			AuthenticationFunc:    openapi3filter.NoopAuthenticationFunc,
			IncludeResponseStatus: true,
			MultiError:            true,
			SkipSettingDefaults:   true,
		},
	}, nil
}

// Exercise sends req to handler and validates both sides of the exchange. The
// expected operation id prevents a path from silently matching the wrong
// operation. Invalid requests are useful for declared 400/413 responses.
func (v *Validator) Exercise(
	ctx context.Context,
	handler http.Handler,
	req *http.Request,
	expectedOperationID string,
	expectedStatus int,
	expectedRequest RequestExpectation,
) (*httptest.ResponseRecorder, error) {
	validationRequest, err := cloneRequest(req)
	if err != nil {
		return nil, err
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if err := v.ValidateExchange(ctx, validationRequest, response, expectedOperationID, expectedStatus, expectedRequest); err != nil {
		return recorder, err
	}
	return recorder, nil
}

// ValidateExchange validates an exchange captured from any HTTP client. This
// is the entry point used by integration/E2E harnesses that do not own the
// in-process handler. Both bodies are restored after validation.
func (v *Validator) ValidateExchange(
	ctx context.Context,
	req *http.Request,
	response *http.Response,
	expectedOperationID string,
	expectedStatus int,
	expectedRequest RequestExpectation,
) error {
	validationRequest, err := cloneRequest(req)
	if err != nil {
		return err
	}
	route, pathParams, err := v.router.FindRoute(validationRequest)
	if err != nil {
		return fmt.Errorf("resolve OpenAPI route: %w", err)
	}
	if route.Operation.OperationID != expectedOperationID {
		return fmt.Errorf("operationId = %q, want %q", route.Operation.OperationID, expectedOperationID)
	}
	input := &openapi3filter.RequestValidationInput{
		Request:    validationRequest,
		PathParams: pathParams,
		Route:      route,
		Options:    v.options,
	}
	requestErr := openapi3filter.ValidateRequest(ctx, input)
	if expectedRequest == RequestValid && requestErr != nil {
		return fmt.Errorf("validate %s request: %w", expectedOperationID, requestErr)
	}
	if expectedRequest == RequestInvalid && requestErr == nil {
		return fmt.Errorf("%s request unexpectedly satisfies the OpenAPI contract", expectedOperationID)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read %s response: %w", expectedOperationID, err)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	if response.StatusCode != expectedStatus {
		return fmt.Errorf("%s status = %d, want %d; body=%s",
			expectedOperationID, response.StatusCode, expectedStatus, body)
	}
	responseInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input,
		Status:                 response.StatusCode,
		Header:                 response.Header,
		Options:                v.options,
	}
	responseInput.SetBodyBytes(body)
	if err := openapi3filter.ValidateResponse(ctx, responseInput); err != nil {
		return fmt.Errorf("validate %s response %d: %w", expectedOperationID, response.StatusCode, err)
	}
	return nil
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.Body == nil {
		return clone, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return clone, nil
}
