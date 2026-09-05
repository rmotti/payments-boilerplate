// Package httpserver owns the inbound HTTP server and middleware chain.
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apispec "github.com/rmotti/payments-boilerplate/api"
	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
	"github.com/swaggest/swgui/v5emb"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"
)

const correlationHeader = "X-Correlation-ID"

type correlationKey struct{}

// Config controls HTTP lifecycle and optional documentation routes.
type Config struct {
	Address         string
	ShutdownTimeout time.Duration
	DocsEnabled     bool
}

// Server is a gracefully stoppable HTTP server.
type Server struct {
	server          *http.Server
	shutdownTimeout time.Duration
}

// New creates an HTTP server with readiness, correlation and telemetry.
func New(cfg Config, logger *zap.Logger, apiHandler openapi.StrictServerInterface) *Server {
	mux := http.NewServeMux()
	strictHandler := openapi.NewStrictHandler(apiHandler, nil)
	openapi.HandlerFromMux(strictHandler, mux)

	if cfg.DocsEnabled {
		mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write(apispec.OpenAPI)
		})
		mux.Handle("GET /docs/", v5emb.New(
			"Payments Boilerplate API",
			"/openapi.yaml",
			"/docs/",
		))
		mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
		})
	}

	base := accessLogMiddleware(logger, recoveryMiddleware(logger, mux))
	instrumented := otelhttp.NewHandler(base, "http.server")
	handler := correlationMiddleware(instrumented)

	return &Server{
		server: &http.Server{
			Addr:              cfg.Address,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		shutdownTimeout: cfg.ShutdownTimeout,
	}
}

// Run serves until ctx is cancelled or the listener fails.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown http: %w", err)
		}
		return nil
	}
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
				logging.WithTrace(r.Context(), logger).Error("http handler panic",
					zap.Any("panic", recovered),
					zap.String("correlation_id", correlationIDFromContext(r.Context())),
				)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
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
