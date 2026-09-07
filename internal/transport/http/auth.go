package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

const apiKeyHeader = "X-API-Key"

// ErrUnauthorized deliberately covers both absent and invalid credentials so
// callers cannot use the response to learn anything about configured keys.
var ErrUnauthorized = errors.New("authentication required")

// APIKeyVerifier is the credential capability required by the HTTP boundary.
type APIKeyVerifier interface {
	Valid(candidate string) bool
}

// apiKeyAuthenticationMiddleware protects every OpenAPI operation unless it
// appears in the deliberately small public allowlist.
func apiKeyAuthenticationMiddleware(verifier APIKeyVerifier) openapi.StrictMiddlewareFunc {
	return func(next openapi.StrictHandlerFunc, operationID string) openapi.StrictHandlerFunc {
		if publicOperation(operationID) {
			return next
		}
		return func(
			ctx context.Context,
			w http.ResponseWriter,
			r *http.Request,
			request any,
		) (any, error) {
			if verifier == nil || !verifier.Valid(r.Header.Get(apiKeyHeader)) {
				return nil, ErrUnauthorized
			}
			return next(ctx, w, r, request)
		}
	}
}

func publicOperation(operationID string) bool {
	return operationID == "GetHealth"
}
