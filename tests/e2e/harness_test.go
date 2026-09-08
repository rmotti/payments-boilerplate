//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rmotti/payments-boilerplate/internal/adapters/rabbitmq"
	paymentapp "github.com/rmotti/payments-boilerplate/internal/application/payments"
	"github.com/rmotti/payments-boilerplate/internal/contracttest"
	paymentdomain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	apiruntime "github.com/rmotti/payments-boilerplate/internal/runtime/api"
	workerruntime "github.com/rmotti/payments-boilerplate/internal/runtime/worker"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	databaseURLEnv = "E2E_DATABASE_URL"
	rabbitURLEnv   = "E2E_RABBITMQ_URL"
	apiKeyA        = "e2e_integration_key_a_0123456789abcdef"
	apiKeyB        = "e2e_integration_key_b_0123456789abcdef"
	webhookSecret  = "whsec_e2e_local_only"
	pollDeadline   = 15 * time.Second
)

type harness struct {
	t             *testing.T
	db            *sql.DB
	databaseURL   string
	rabbitURL     string
	provider      *fakePaymentProvider
	contract      *contracttest.Validator
	apiBaseURL    string
	apiCancel     context.CancelFunc
	apiDone       <-chan error
	workerBaseURL string
	workerCancel  context.CancelFunc
	workerDone    <-chan error
	logs          *safeBuffer
	// environment and docsEnabled are what a deployment configures. They are
	// harness fields, not constants, because the documentation policy is a
	// function of both and each combination is a different process.
	environment string
	docsEnabled bool
}

func newHarness(t *testing.T, startWorker bool) *harness {
	t.Helper()
	return newConfiguredHarness(t, startWorker, "test", false)
}

func newConfiguredHarness(t *testing.T, startWorker bool, environment string, docsEnabled bool) *harness {
	t.Helper()
	databaseURL := os.Getenv(databaseURLEnv)
	rabbitURL := os.Getenv(rabbitURLEnv)
	if databaseURL == "" || rabbitURL == "" {
		t.Skipf("%s and %s are required", databaseURLEnv, rabbitURLEnv)
	}
	if err := validateCleanupTargets(os.Getenv(destructiveCleanupConsent), databaseURL, rabbitURL); err != nil {
		t.Fatalf("refusing destructive e2e setup: %v", err)
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping postgres: %v", err)
	}
	root := repositoryRoot(t)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("configure migrations: %v", err)
	}
	if err := goose.UpContext(ctx, db, filepath.Join(root, "db/migrations")); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	contract, err := contracttest.New(ctx, filepath.Join(root, "api/openapi.yaml"))
	if err != nil {
		t.Fatalf("load OpenAPI contract: %v", err)
	}
	h := &harness{
		t: t, db: db, databaseURL: databaseURL, rabbitURL: rabbitURL,
		provider: &fakePaymentProvider{}, contract: contract, logs: &safeBuffer{},
		environment: environment, docsEnabled: docsEnabled,
	}
	h.cleanupState()
	t.Cleanup(func() {
		h.stop()
		h.cleanupState()
		if err := db.Close(); err != nil {
			t.Errorf("close postgres: %v", err)
		}
	})
	h.startAPI()
	if startWorker {
		h.startWorker()
	}
	return h
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate e2e source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func (h *harness) runtimeConfig(service, address string) config.Config {
	return config.Config{
		ServiceName: service, Environment: h.environment, HTTPAddress: address,
		DocsEnabled: h.docsEnabled,
		DatabaseURL: h.databaseURL, DatabaseMaxOpenConnections: 8,
		DatabaseMaxIdleConnections: 4, DatabaseConnectionMaxLifetime: time.Minute,
		RabbitMQURL: h.rabbitURL, IntegrationAPIKeys: []string{apiKeyA, apiKeyB},
		StripeWebhookSecret: webhookSecret, LogLevel: "debug", LogFormat: "console",
		StartupTimeout: 10 * time.Second, ShutdownTimeout: 3 * time.Second,
		OutboxBatchSize: 1, OutboxInterval: 25 * time.Millisecond,
		OutboxLeaseDuration: 10 * time.Second, OutboxBackoffBase: 25 * time.Millisecond,
		OutboxBackoffMax: 100 * time.Millisecond, OutboxAlertAfterAttempts: 3,
		ConsumerConcurrency: 2, ConsumerPrefetch: 2, ConsumerMaxAttempts: 2,
		ConsumerRetryDelays: []time.Duration{100 * time.Millisecond, 250 * time.Millisecond},
	}
}

func (h *harness) logger() *zap.Logger {
	encoder := zap.NewDevelopmentEncoderConfig()
	core := zap.New(zapcore.NewCore(zapcore.NewConsoleEncoder(encoder), zapcore.AddSync(h.logs), zap.DebugLevel))
	return core
}

func (h *harness) startAPI() {
	h.t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatalf("listen API: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := h.runtimeConfig(apiruntime.ServiceName, listener.Addr().String())
	go func() {
		done <- apiruntime.Run(ctx, cfg, apiruntime.Options{Listener: listener, PaymentProvider: h.provider, Logger: h.logger()})
	}()
	h.apiBaseURL = "http://" + listener.Addr().String()
	h.apiCancel, h.apiDone = cancel, done
	h.waitHTTPReady(h.apiBaseURL + "/health")
}

func (h *harness) startWorker() {
	h.t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatalf("listen worker: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := h.runtimeConfig(workerruntime.ServiceName, listener.Addr().String())
	go func() {
		done <- workerruntime.Run(ctx, cfg, workerruntime.Options{Listener: listener, Logger: h.logger(), InstanceID: "e2e-worker"})
	}()
	h.workerBaseURL = "http://" + listener.Addr().String()
	h.workerCancel, h.workerDone = cancel, done
	h.waitHTTPReady(h.workerBaseURL + "/health")
}

func (h *harness) waitHTTPReady(endpoint string) {
	h.t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	h.poll("HTTP readiness for "+endpoint, func() (bool, string, error) {
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
		response, err := client.Do(request)
		if err != nil {
			return false, err.Error(), nil
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK, response.Status, nil
	})
}

func (h *harness) stop() {
	for _, process := range []struct {
		name   string
		cancel context.CancelFunc
		done   <-chan error
	}{{"worker", h.workerCancel, h.workerDone}, {"api", h.apiCancel, h.apiDone}} {
		if process.cancel == nil {
			continue
		}
		process.cancel()
		select {
		case err := <-process.done:
			if err != nil {
				h.t.Errorf("%s shutdown: %v\nlogs:\n%s", process.name, err, h.logs.String())
			}
		case <-time.After(8 * time.Second):
			h.t.Errorf("%s did not stop\nlogs:\n%s", process.name, h.logs.String())
		}
	}
	h.workerCancel, h.apiCancel = nil, nil
}

func (h *harness) cleanupState() {
	h.t.Helper()
	if err := validateCleanupTargets(os.Getenv(destructiveCleanupConsent), h.databaseURL, h.rabbitURL); err != nil {
		h.t.Fatalf("refusing cleanup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The table names are constants, never input or a wildcard. CASCADE is not
	// used, so adding a new dependent table makes cleanup fail visibly.
	const truncateKnownTables = "TRUNCATE outbox_events, webhook_events, payment_attempts, payments, orders"
	if _, err := h.db.ExecContext(ctx, truncateKnownTables); err != nil {
		h.t.Fatalf("truncate known e2e tables: %v", err)
	}

	connection, err := amqp.Dial(h.rabbitURL)
	if err != nil {
		h.t.Fatalf("connect RabbitMQ for cleanup: %v", err)
	}
	defer func() { _ = connection.Close() }()
	channel, err := connection.Channel()
	if err != nil {
		h.t.Fatalf("open RabbitMQ cleanup channel: %v", err)
	}
	defer func() { _ = channel.Close() }()
	if err := rabbitmq.DeclareTopology(channel); err != nil {
		h.t.Fatalf("declare known RabbitMQ topology: %v", err)
	}
	tiers := []rabbitmq.RetryTier{{Delay: 100 * time.Millisecond}, {Delay: 250 * time.Millisecond}}
	if err := rabbitmq.DeclareRetryTopology(channel, tiers); err != nil {
		h.t.Fatalf("declare retry topology: %v", err)
	}
	queues := []string{rabbitmq.WebhooksQueue, rabbitmq.DeadLetterQueue}
	for _, tier := range tiers {
		queues = append(queues, tier.Queue())
	}
	for _, queue := range queues {
		if _, err := channel.QueuePurge(queue, false); err != nil {
			h.t.Fatalf("purge known queue %s: %v", queue, err)
		}
	}
}

type capturedResponse struct {
	StatusCode int
	Header     http.Header
}

func (h *harness) request(method, path, key string, body any, headers map[string]string) (capturedResponse, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		switch value := body.(type) {
		case []byte:
			reader = bytes.NewReader(value)
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				h.t.Fatalf("encode request: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	}
	request, err := http.NewRequestWithContext(context.Background(), method, h.apiBaseURL+path, reader)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("X-API-Key", key)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	validationRequest := request.Clone(request.Context())
	if request.GetBody != nil {
		validationRequest.Body, err = request.GetBody()
		if err != nil {
			h.t.Fatalf("clone request body for contract validation: %v", err)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("%s %s: %v\nlogs:\n%s", method, path, err, h.logs.String())
	}
	operationID, expectation := contractOperation(method, path, headers)
	if err := h.contract.ValidateExchange(context.Background(), validationRequest, response,
		operationID, response.StatusCode, expectation); err != nil {
		_ = response.Body.Close()
		h.t.Fatalf("OpenAPI exchange validation: %v", err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		h.t.Fatalf("read response: %v", err)
	}
	return capturedResponse{StatusCode: response.StatusCode, Header: response.Header.Clone()}, data
}

// rawRequest reaches a path directly, without mapping it to an OpenAPI
// operation. Documentation routes and unregistered paths are outside the
// contract, so they have no operation to validate against.
func (h *harness) rawRequest(baseURL, method, path, key string) (capturedResponse, []byte) {
	h.t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, nil)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if key != "" {
		request.Header.Set("X-API-Key", key)
	}
	// Redirects are part of what is under test: /docs must answer with the
	// redirect itself, and following it would report the target's status.
	// Keep-alive is off because an idle pooled connection would still be open
	// when the process is stopped and would consume the whole graceful
	// shutdown deadline.
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		h.t.Fatalf("%s %s: %v\nlogs:\n%s", method, path, err, h.logs.String())
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		h.t.Fatalf("read response: %v", err)
	}
	return capturedResponse{StatusCode: response.StatusCode, Header: response.Header.Clone()}, data
}

func contractOperation(method, path string, headers map[string]string) (string, contracttest.RequestExpectation) {
	cleanPath := path
	if index := strings.IndexByte(cleanPath, '?'); index >= 0 {
		cleanPath = cleanPath[:index]
	}
	expectation := contracttest.RequestValid
	switch {
	case method == http.MethodGet && cleanPath == "/health":
		return "getHealth", expectation
	case method == http.MethodPost && cleanPath == "/v1/orders":
		if headers["Idempotency-Key"] == "" {
			expectation = contracttest.RequestInvalid
		}
		return "createOrder", expectation
	case method == http.MethodGet && strings.HasPrefix(cleanPath, "/v1/orders/"):
		return "getOrder", expectation
	case method == http.MethodPost && strings.HasSuffix(cleanPath, "/checkout"):
		if headers["Idempotency-Key"] == "" {
			expectation = contracttest.RequestInvalid
		}
		return "createCheckout", expectation
	case method == http.MethodGet && cleanPath == "/v1/webhook-events":
		return "listWebhookEvents", expectation
	case method == http.MethodPost && strings.HasPrefix(cleanPath, "/v1/webhook-events/"):
		return "reprocessWebhookEvent", expectation
	case method == http.MethodPost && cleanPath == "/v1/webhooks/stripe":
		return "receiveStripeWebhook", expectation
	default:
		panic("e2e request has no OpenAPI operation: " + method + " " + path)
	}
}

func (h *harness) poll(description string, observe func() (bool, string, error)) {
	h.t.Helper()
	deadline := time.Now().Add(pollDeadline)
	last := "not observed"
	for time.Now().Before(deadline) {
		done, diagnostic, err := observe()
		if err != nil {
			h.t.Fatalf("poll %s: %v; last=%s\nlogs:\n%s", description, err, last, h.logs.String())
		}
		last = diagnostic
		if done {
			return
		}
		timer := time.NewTimer(25 * time.Millisecond)
		<-timer.C
	}
	h.t.Fatalf("deadline waiting for %s; last=%s\nlogs:\n%s", description, last, h.logs.String())
}

type fakePaymentProvider struct {
	mu       sync.Mutex
	requests []paymentapp.ProviderRequest
	sessions []paymentdomain.Session
	sequence int
}

// safeBuffer keeps diagnostics race-free while API, relay and consumer log
// from independent goroutines.
type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *safeBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (f *fakePaymentProvider) CreateCheckout(_ context.Context, request paymentapp.ProviderRequest) (paymentdomain.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	f.sequence++
	session := paymentdomain.Session{
		ID: fmt.Sprintf("cs_e2e_%d", f.sequence), PaymentIntentID: fmt.Sprintf("pi_e2e_%d", f.sequence),
		URL: fmt.Sprintf("https://checkout.stripe.test/e2e/%d", f.sequence), ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
	f.sessions = append(f.sessions, session)
	return session, nil
}

func (f *fakePaymentProvider) snapshot() ([]paymentapp.ProviderRequest, []paymentdomain.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]paymentapp.ProviderRequest(nil), f.requests...), append([]paymentdomain.Session(nil), f.sessions...)
}

func stripeSignature(payload []byte, secret string, at time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.%s", at.Unix(), payload)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func (h *harness) postWebhook(payload []byte, valid bool) (capturedResponse, []byte) {
	h.t.Helper()
	secret := webhookSecret
	if !valid {
		secret = "whsec_wrong"
	}
	return h.request(http.MethodPost, "/v1/webhooks/stripe", "", payload, map[string]string{
		"Content-Type": "application/octet-stream", "Stripe-Signature": stripeSignature(payload, secret, time.Now()),
	})
}

func requireStatus(t *testing.T, response capturedResponse, body []byte, want int) {
	t.Helper()
	if response.StatusCode != want {
		t.Fatalf("status = %d, want %d; body=%s", response.StatusCode, want, body)
	}
}

func scanCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("query count: %v", err)
	}
	return count
}
