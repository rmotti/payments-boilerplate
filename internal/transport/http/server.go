// Package httpserver owns the inbound HTTP server and middleware chain.
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"
)

const (
	correlationHeader = "X-Correlation-ID"

	// maxBodyBytes bounds request bodies. The public API only receives small
	// JSON documents; anything larger is rejected before it is decoded.
	maxBodyBytes = 64 << 10

	// webhookPath is the one route whose body is not written by the
	// integrator, so its size is not ours to keep small.
	webhookPath = "/v1/webhooks/stripe"

	// healthPath is the probe. The rate limiting policy gives it a bucket of
	// its own so ordinary traffic cannot make a deployment look unhealthy.
	healthPath = "/health"

	// webhookMaxBodyBytes gives provider events their own headroom. An event
	// rejected for size cannot have its signature verified, and treating that
	// as a client error would turn a local misconfiguration into permanent
	// event loss, so the limit is deliberately generous and the overflow is
	// answered with 500 so the provider redelivers.
	webhookMaxBodyBytes = 512 << 10
)

type correlationKey struct{}

// Config controls HTTP lifecycle, the documentation policy and which peers
// may speak for their clients.
type Config struct {
	Address         string
	ShutdownTimeout time.Duration

	// Docs is the documentation policy. The zero value registers no
	// documentation route; see DocsModeFor for how configuration maps to it.
	Docs DocsMode

	// TrustedProxies are the networks whose X-Forwarded-For header identifies
	// the client. Empty means the TCP peer is always the client.
	TrustedProxies []netip.Prefix

	// RateLimit is the rate limiting policy. A zero value, whose Enabled is
	// false, builds a server with no limiter at all.
	RateLimit RateLimitConfig

	// Listener, when set, is served instead of binding Address. A caller that
	// already holds an open socket avoids the race of picking a free port and
	// then trying to bind it again, which is what lets a test on port zero
	// learn its own address before the server starts.
	Listener net.Listener
}

// Server is a gracefully stoppable HTTP server.
type Server struct {
	server          *http.Server
	listener        net.Listener
	shutdownTimeout time.Duration
}

// New creates the API server: every operation of the contract, guarded by
// API key authentication, plus the documentation routes the policy allows.
//
// It panics on an invalid rate limiting policy, which cannot happen through
// the binaries: configuration is validated at load, long before this point.
// NewWithRateLimits is the form that returns the error instead.
func New(
	cfg Config,
	logger *zap.Logger,
	apiHandler openapi.StrictServerInterface,
	apiKeyVerifier APIKeyVerifier,
) *Server {
	fingerprinter, _ := apiKeyVerifier.(CredentialFingerprinter)
	server, err := NewWithRateLimits(cfg, logger, apiHandler, apiKeyVerifier, fingerprinter, nil)
	if err != nil {
		panic("httpserver: invalid rate limit configuration: " + err.Error())
	}
	return server
}

// NewWithRateLimits creates the API server with rate limiting wired in.
//
// The fingerprinter is what lets an authenticated operation be counted per
// credential without this package ever handling a key: it answers only for
// credentials that are actually configured. It is required when rate limiting
// is enabled. The observer records refusals and may be nil when metrics are not
// needed by the caller.
func NewWithRateLimits(
	cfg Config,
	logger *zap.Logger,
	apiHandler openapi.StrictServerInterface,
	apiKeyVerifier APIKeyVerifier,
	fingerprinter CredentialFingerprinter,
	observer RateLimitObserver,
) (*Server, error) {
	limiters, err := newRateLimiters(cfg.RateLimit, fingerprinter, observer)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	openapi.HandlerWithOptions(
		newStrictHandler(logger, apiHandler, apiKeyVerifier, limiters),
		openapi.StdHTTPServerOptions{
			BaseRouter:       mux,
			ErrorHandlerFunc: requestErrorHandler,
		})
	registerDocs(mux, cfg.Docs, apiKeyVerifier)
	return newServer(cfg, logger, mux, limiters), nil
}

// NewHealthOnly creates the server a process without a public API runs. Only
// GET /health is registered: the other operations of the contract are absent
// from the mux, so they answer 404 like any unknown path, rather than being
// registered only to refuse with 401 or 501. Documentation is never served.
func NewHealthOnly(cfg Config, logger *zap.Logger, healthService *health.Service) *Server {
	mux := http.NewServeMux()
	wrapper := openapi.ServerInterfaceWrapper{
		Handler:          newStrictHandler(logger, NewAPIHandler(healthService, nil, nil, nil), nil, nil),
		ErrorHandlerFunc: requestErrorHandler,
	}
	mux.HandleFunc("GET /health", wrapper.GetHealth)
	// The worker serves only the probe, so it carries no limiter: the health
	// bucket exists to keep other traffic from starving probes, and there is
	// no other traffic here.
	return newServer(cfg, logger, mux, nil)
}

// newStrictHandler orders the two strict middlewares deliberately. The
// generated wrapper applies them in reverse, so listing rate limiting last
// makes it the outermost of the pair: a request over its credential limit is
// refused before the credential is compared again and before the handler runs.
// Only a credential the fingerprinter already recognizes ever reaches a bucket,
// so nothing here weakens the authentication that follows it.
func newStrictHandler(
	logger *zap.Logger,
	apiHandler openapi.StrictServerInterface,
	apiKeyVerifier APIKeyVerifier,
	limiters *rateLimiters,
) openapi.ServerInterface {
	return openapi.NewStrictHandlerWithOptions(apiHandler, []openapi.StrictMiddlewareFunc{
		apiKeyAuthenticationMiddleware(apiKeyVerifier),
		operationRateLimitMiddleware(limiters),
	}, openapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  requestErrorHandler,
		ResponseErrorHandlerFunc: responseErrorHandler(logger),
	})
}

// newServer wraps the routes in the middleware chain shared by every process.
// Correlation and the security headers sit outermost so that every response,
// including a 404 from the mux and a 500 from the recovery path, carries them.
// The pre-routing limiter sits directly inside client address resolution and
// outside everything else: routing, the body limit, OpenAPI decode and
// authentication all happen after it. Business and documentation routes use
// the coarse address bucket, health uses its dedicated address bucket, and the
// webhook uses its global provider bucket. A refusal therefore costs one lookup
// rather than a parse and a credential comparison.
func newServer(cfg Config, logger *zap.Logger, mux *http.ServeMux, limiters *rateLimiters) *Server {
	base := accessLogMiddleware(logger, recoveryMiddleware(logger, bodyLimitMiddleware(mux)))
	instrumented := otelhttp.NewHandler(base, "http.server")
	limited := clientRateLimitMiddleware(limiters, instrumented)
	resolver := NewClientAddressResolver(cfg.TrustedProxies)
	handler := correlationMiddleware(securityHeadersMiddleware(clientAddressMiddleware(resolver, limited)))

	return &Server{
		server: &http.Server{
			Addr:              cfg.Address,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		listener:        cfg.Listener,
		shutdownTimeout: cfg.ShutdownTimeout,
	}
}

// Run serves until ctx is cancelled or the listener fails.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if s.listener != nil {
			errCh <- s.server.Serve(s.listener)
			return
		}
		errCh <- s.server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return serveError(err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		shutdownErr := s.server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			// Shutdown leaves active connections open when its deadline expires.
			// Close is the forced fallback that makes Run keep its lifecycle
			// promise even when one handler cannot drain in time.
			closeErr := s.server.Close()
			return errors.Join(
				fmt.Errorf("shutdown http: %w", shutdownErr),
				closeErr,
				serveError(<-errCh),
			)
		}
		// Shutdown closes the listener, but wait for Serve itself to return so no
		// server goroutine remains after Run reports completion.
		return serveError(<-errCh)
	}
}

func serveError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve http: %w", err)
}

func correlationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := strings.TrimSpace(r.Header.Get(correlationHeader))
		if correlationID == "" || len(correlationID) > 128 {
			correlationID = newCorrelationID()
		}
		w.Header().Set(correlationHeader, correlationID)
		ctx := context.WithValue(r.Context(), correlationKey{}, correlationID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func accessLogMiddleware(logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(response, r)

		requestLogger := logging.WithTrace(r.Context(), logger)
		requestLogger.Info("http request completed",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", response.status),
			zap.Duration("duration", time.Since(started)),
			zap.String("correlation_id", correlationIDFromContext(r.Context())),
		)
	})
}

func recoveryMiddleware(logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				// recovered can be anything a panic carried, including a formatted
				// string built from a driver or client error; its text is
				// sanitized the same way a logged error is, rather than trusted
				// because it happens to not be an error value.
				logging.WithTrace(r.Context(), logger).Error("http handler panic",
					zap.String("panic", errsanitize.Sanitize(fmt.Sprint(recovered))),
					zap.String("correlation_id", correlationIDFromContext(r.Context())),
				)
				writeError(w, r, http.StatusInternalServerError, codeInternalError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// bodyLimitMiddleware caps the bytes a handler can read from the request body.
// It runs before routing, so the limit is chosen from the path.
func bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, bodyLimitFor(r.URL.Path))
		}
		next.ServeHTTP(w, r)
	})
}

func bodyLimitFor(path string) int64 {
	if path == webhookPath {
		return webhookMaxBodyBytes
	}
	return maxBodyBytes
}

// requestErrorHandler answers failures that happen before the operation runs:
// a missing required header, a malformed parameter or an undecodable body.
func requestErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, r, http.StatusRequestEntityTooLarge, codeInvalidRequest, "request body too large")
		return
	}
	writeError(w, r, http.StatusBadRequest, codeInvalidRequest, err.Error())
}

// responseErrorHandler answers errors returned by the operation itself. Those
// are unexpected by construction, so the cause is logged with the correlation
// id and the client receives only a generic body.
func responseErrorHandler(logger *zap.Logger) func(w http.ResponseWriter, r *http.Request, err error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		var limited rateLimitedError
		if errors.As(err, &limited) {
			writeRateLimited(w, r, limited.retryAfter)
			return
		}
		if errors.Is(err, ErrUnauthorized) {
			writeError(w, r, http.StatusUnauthorized, codeUnauthorized, ErrUnauthorized.Error())
			return
		}
		if errors.Is(err, ErrNotServed) {
			writeError(w, r, http.StatusNotImplemented, codeNotImplemented, ErrNotServed.Error())
			return
		}
		logging.WithTrace(r.Context(), logger).Error("http handler failed",
			logging.SanitizedError(err),
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("correlation_id", correlationIDFromContext(r.Context())),
		)
		writeError(w, r, http.StatusInternalServerError, codeInternalError, "internal error")
	}
}

func correlationIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(correlationKey{}).(string)
	return value
}

func newCorrelationID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value)
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
