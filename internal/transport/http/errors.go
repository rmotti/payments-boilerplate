package httpserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

// Stable error codes exposed by the contract.
const (
	codeInvalidRequest         = "invalid_request"
	codeUnauthorized           = "unauthorized"
	codeInvalidSignature       = "invalid_signature"
	codeProductNotFound        = "product_not_found"
	codeOrderNotFound          = "order_not_found"
	codeIdempotencyKeyConflict = "idempotency_key_conflict"
	codeCheckoutInProgress     = "checkout_in_progress"
	codeOrderNotPayable        = "order_not_payable"
	codeProviderUnavailable    = "provider_unavailable"
	codeNotImplemented         = "not_implemented"
	codeInternalError          = "internal_error"
)

// newError builds the common error body, carrying the request correlation id
// so a client can quote it back to whoever operates the API.
func newError(ctx context.Context, code, message string) openapi.Error {
	return openapi.Error{
		Code:          code,
		Message:       message,
		CorrelationId: correlationIDFromContext(ctx),
	}
}

// writeError renders an error outside the strict handler, for failures that
// happen before or after the operation runs (binding, decoding, panics).
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(newError(r.Context(), code, message))
}
