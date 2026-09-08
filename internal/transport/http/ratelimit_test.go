package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	webhookapp "github.com/rmotti/payments-boilerplate/internal/application/webhooks"
	orderdomain "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	"github.com/rmotti/payments-boilerplate/internal/platform/auth"
	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"github.com/rmotti/payments-boilerplate/internal/platform/ratelimit"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
	"go.uber.org/zap"
)

// testClock is the fake clock every test here uses. Time only moves when a
// test says so, which is what makes refill and expiry exact rather than
// approximate, and keeps the suite free of sleeps.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recordingObserver captures what the limiters reported, so a test can assert
// both that a refusal was counted and that nothing high-cardinality reached
// the metric.
type recordingObserver struct {
	mu      sync.Mutex
	entries []observation
}

type observation struct{ limiter, routeClass string }

func (o *recordingObserver) Rejected(limiter, routeClass string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.entries = append(o.entries, observation{limiter: limiter, routeClass: routeClass})
}

func (o *recordingObserver) snapshot() []observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]observation(nil), o.entries...)
}

// countingOrders records whether the business use case was reached at all. A
// rate limited request must never increment it.
type countingOrders struct {
	created atomic.Int64
	order   orderdomain.Order
}

func (c *countingOrders) Create(context.Context, orderapp.CreateInput) (orderdomain.Order, error) {
	c.created.Add(1)
	return c.order, nil
}

func (c *countingOrders) Get(context.Context, string) (orderdomain.Order, error) {
	c.created.Add(1)
	return c.order, nil
}

// testRateLimitConfig is a policy small enough to exhaust in a few requests,
// driven by the injected clock.
func testRateLimitConfig(clock *testClock) RateLimitConfig {
	return RateLimitConfig{
		Enabled:            true,
		Client:             ratelimit.Policy{Burst: 3, Interval: 3 * time.Second},
		ClientCapacity:     16,
		Credential:         ratelimit.Policy{Burst: 2, Interval: 2 * time.Second},
		CredentialCapacity: 8,
		Webhook:            ratelimit.Policy{Burst: 2, Interval: 2 * time.Second},
		Health:             ratelimit.Policy{Burst: 4, Interval: 4 * time.Second},
		HealthCapacity:     8,
		IdleTTL:            time.Minute,
		Clock:              clock.Now,
	}
}

// rateLimitedServer builds the real server, with the real middleware chain, so
// the tests exercise ordering rather than a reconstruction of it.
type rateLimitedServer struct {
	handler  http.Handler
	orders   *countingOrders
	observer *recordingObserver
}

func newRateLimitedServer(t *testing.T, cfg RateLimitConfig, trusted []netip.Prefix) rateLimitedServer {
	t.Helper()
	orders := &countingOrders{order: orderdomain.Order{
		ID: "ord_0123456789abcdef0123456789abcdef", Status: orderdomain.StatusPending,
		Amount: 10000, Currency: orderdomain.BRL,
	}}
	observer := &recordingObserver{}
	verifier, err := auth.NewAPIKeyVerifier([]string{testAPIKey, secondTestAPIKey})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	api := NewAPIHandler(
		health.New("payments-test", "test", map[string]health.Checker{
			"postgres": func(context.Context) error { return nil },
		}),
		orders, nil, concurrentWebhooks{}, nil)
	server, err := NewWithRateLimits(Config{
		Address:         ":0",
		ShutdownTimeout: time.Second,
		TrustedProxies:  trusted,
		RateLimit:       cfg,
	}, zap.NewNop(), api, verifier, verifier, observer)
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	return rateLimitedServer{handler: server.server.Handler, orders: orders, observer: observer}
}

// do sends one request through the whole chain from the given peer.
func (s rateLimitedServer) do(t *testing.T, method, path, peer string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if method == http.MethodPost && strings.HasPrefix(path, "/v1/orders") && !strings.HasSuffix(path, "checkout") {
		body = `{"productId":"product_demo","quantity":1}`
	}
	request := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "rate-limit-test")
	}
	request.RemoteAddr = peer
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	s.handler.ServeHTTP(recorder, request)
	return recorder
}

const secondTestAPIKey = "second-api-key-with-at-least-32-characters"

// authenticated is the header set a valid integrator sends.
func authenticated(key string) map[string]string {
	return map[string]string{apiKeyHeader: key}
}

// TestClientLimiterConsumesBurstThenRefills is the core of the policy: a
// caller spends its burst, is refused, and is served again once the clock has
// advanced far enough for a token to accrue. No sleep is involved.
func TestClientLimiterConsumesBurstThenRefills(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	// Take the credential limit out of the way so this test speaks only about
	// the coarse one.
	cfg.Credential = ratelimit.Policy{Burst: 1000, Interval: time.Second}
	server := newRateLimitedServer(t, cfg, nil)

	const peer = "203.0.113.10:5000"
	for attempt := 1; attempt <= cfg.Client.Burst; attempt++ {
		if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusCreated {
			t.Fatalf("request %d status = %d, want %d", attempt, got, http.StatusCreated)
		}
	}

	refused := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey))
	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("status after burst = %d, want %d", refused.Code, http.StatusTooManyRequests)
	}
	// A refused request must not reach the use case.
	if created := server.orders.created.Load(); created != int64(cfg.Client.Burst) {
		t.Fatalf("use case ran %d times, want %d", created, cfg.Client.Burst)
	}

	// One token is worth Interval/Burst. Just short of it still refuses.
	tokenInterval := cfg.Client.Interval / time.Duration(cfg.Client.Burst)
	clock.Advance(tokenInterval - time.Millisecond)
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusTooManyRequests {
		t.Fatalf("status before a token accrued = %d, want %d", got, http.StatusTooManyRequests)
	}

	clock.Advance(time.Millisecond)
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusCreated {
		t.Fatalf("status after refill = %d, want %d", got, http.StatusCreated)
	}

	// An idle caller accumulates at most one burst, never more.
	clock.Advance(cfg.Client.Interval * 10)
	for attempt := 1; attempt <= cfg.Client.Burst; attempt++ {
		if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusCreated {
			t.Fatalf("request %d after a long idle = %d, want %d", attempt, got, http.StatusCreated)
		}
	}
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusTooManyRequests {
		t.Fatalf("burst was not capped after a long idle: status = %d", got)
	}
}

// TestRateLimitedResponseCarriesRetryAfterAndEnvelope covers the shape of the
// refusal: the project's error envelope, the correlation id, the security
// headers and a Retry-After a client can act on.
func TestRateLimitedResponseCarriesRetryAfterAndEnvelope(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	cfg.Credential = ratelimit.Policy{Burst: 1000, Interval: time.Second}
	server := newRateLimitedServer(t, cfg, nil)

	const peer = "203.0.113.11:5000"
	for attempt := 0; attempt < cfg.Client.Burst; attempt++ {
		server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey))
	}
	refused := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey))

	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", refused.Code, http.StatusTooManyRequests)
	}
	retryAfter := refused.Header().Get(retryAfterHeader)
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After = %q, want whole seconds: %v", retryAfter, err)
	}
	if seconds < 1 {
		t.Fatalf("Retry-After = %d, want at least 1 so a client does not retry immediately", seconds)
	}
	// The wait must never exceed the window a whole burst refills in.
	if maximum := int(cfg.Client.Interval / time.Second); seconds > maximum {
		t.Fatalf("Retry-After = %d, want at most %d", seconds, maximum)
	}

	if got := refused.Header().Get(correlationHeader); got == "" {
		t.Error("rate limited response carries no correlation header")
	}
	// The strict security headers apply to a 429 like to any other response.
	for header, want := range map[string]string{
		headerContentTypeOptions: "nosniff",
		headerReferrerPolicy:     "no-referrer",
		headerCacheControl:       "no-store",
		headerFrameOptions:       "DENY",
		headerContentSecurity:    strictContentSecurityPolicy,
	} {
		if got := refused.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if got := refused.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		CorrelationID string `json:"correlationId"`
	}
	if err := json.Unmarshal(refused.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != codeRateLimited {
		t.Errorf("code = %q, want %q", body.Code, codeRateLimited)
	}
	if body.CorrelationID != refused.Header().Get(correlationHeader) {
		t.Error("body correlation id does not match the response header")
	}
	// Nothing about the identity that was limited may appear in the body.
	for _, forbidden := range []string{testAPIKey, "203.0.113.11"} {
		if strings.Contains(refused.Body.String(), forbidden) {
			t.Errorf("body leaks %q", forbidden)
		}
	}
}

// TestCredentialLimiterIsolatesCredentials proves the authenticated limit is
// keyed by the credential and not shared: one integrator exhausting its bucket
// must not refuse another one arriving from the same address.
func TestCredentialLimiterIsolatesCredentials(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	// Leave the coarse limit wide, so only the credential limit can bind.
	cfg.Client = ratelimit.Policy{Burst: 1000, Interval: time.Second}
	server := newRateLimitedServer(t, cfg, nil)

	const peer = "203.0.113.12:5000"
	for attempt := 1; attempt <= cfg.Credential.Burst; attempt++ {
		if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusCreated {
			t.Fatalf("first credential request %d = %d, want %d", attempt, got, http.StatusCreated)
		}
	}
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusTooManyRequests {
		t.Fatalf("first credential over its burst = %d, want %d", got, http.StatusTooManyRequests)
	}

	// The second credential, same address, still has its whole burst.
	for attempt := 1; attempt <= cfg.Credential.Burst; attempt++ {
		if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(secondTestAPIKey)).Code; got != http.StatusCreated {
			t.Fatalf("second credential request %d = %d, want %d", attempt, got, http.StatusCreated)
		}
	}
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(secondTestAPIKey)).Code; got != http.StatusTooManyRequests {
		t.Fatalf("second credential over its burst = %d, want %d", got, http.StatusTooManyRequests)
	}
}

// TestInvalidCredentialCreatesNoBucket is the memory guarantee that matters
// most: a caller must not be able to mint a limiter entry per header value it
// invents. Such requests stay accounted for by the coarse limiter alone, and
// authentication refuses them as it always did.
func TestInvalidCredentialCreatesNoBucket(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	cfg.Client = ratelimit.Policy{Burst: 1000, Interval: time.Second}
	server := newRateLimitedServer(t, cfg, nil)

	limiters, err := newRateLimiters(cfg, mustVerifier(t), nil)
	if err != nil {
		t.Fatalf("build limiters: %v", err)
	}
	for attempt := 0; attempt < 50; attempt++ {
		invalid := "forged-key-" + strconv.Itoa(attempt) + "-padded-to-length"
		response := server.do(t, http.MethodPost, "/v1/orders", "203.0.113.13:5000", authenticated(invalid))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid credential status = %d, want %d", response.Code, http.StatusUnauthorized)
		}
		if _, allowed := limiters.allowCredential(invalid, RouteClassBusiness); !allowed {
			t.Fatal("an invalid credential was given a bucket")
		}
	}
	if held := limiters.credential.Len(); held != 0 {
		t.Fatalf("credential limiter holds %d buckets after 50 forged keys, want 0", held)
	}
}

// TestWebhookUsesOneGlobalBucket proves the provider endpoint is limited as a
// whole. Two different source addresses share the bucket, because the caller
// is Stripe and its address is not an identity we can trust.
func TestWebhookUsesOneGlobalBucket(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	// A per-address burst of one proves the webhook does not pay this bucket:
	// both requests below come from the same address and must still pass.
	cfg.Client = ratelimit.Policy{Burst: 1, Interval: time.Hour}
	server := newRateLimitedServer(t, cfg, nil)

	webhookHeaders := map[string]string{
		"Content-Type":     "application/octet-stream",
		"Stripe-Signature": "t=1,v1=abc",
	}
	const firstPeer = "198.51.100.1:443"
	// Spend the whole provider burst from one address. If the client-address
	// limiter also applied here, the second request would already be refused.
	for index := 0; index < cfg.Webhook.Burst; index++ {
		response := server.do(t, http.MethodPost, webhookPath, firstPeer, webhookHeaders)
		if response.Code == http.StatusTooManyRequests {
			t.Fatalf("webhook request %d was refused inside the burst", index+1)
		}
	}
	// A third address, never seen before, finds the shared bucket empty.
	refused := server.do(t, http.MethodPost, webhookPath, "198.51.100.3:443", webhookHeaders)
	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("webhook from a new address = %d, want %d", refused.Code, http.StatusTooManyRequests)
	}
}

// TestWebhookGlobalLimiterDoesNotRequireClientAddress covers local transports
// such as a Unix socket. The webhook policy is global, so absence of an IP must
// not turn rate limiting off for this route.
func TestWebhookGlobalLimiterDoesNotRequireClientAddress(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	server := newRateLimitedServer(t, cfg, nil)
	headers := map[string]string{
		"Content-Type":     "application/octet-stream",
		"Stripe-Signature": "t=1,v1=abc",
	}

	for attempt := 1; attempt <= cfg.Webhook.Burst; attempt++ {
		if got := server.do(t, http.MethodPost, webhookPath, "", headers).Code; got == http.StatusTooManyRequests {
			t.Fatalf("addressless webhook request %d was refused inside the burst", attempt)
		}
	}
	if got := server.do(t, http.MethodPost, webhookPath, "", headers).Code; got != http.StatusTooManyRequests {
		t.Fatalf("addressless webhook over the global burst = %d, want %d", got, http.StatusTooManyRequests)
	}
}

// TestHealthKeepsItsOwnBudget proves the probe is not starved by ordinary
// traffic: a client that has exhausted the business limit can still be probed.
func TestHealthKeepsItsOwnBudget(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	server := newRateLimitedServer(t, cfg, nil)

	const peer = "203.0.113.14:5000"
	for attempt := 0; attempt < cfg.Client.Burst+2; attempt++ {
		server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey))
	}
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusTooManyRequests {
		t.Fatalf("business traffic was not limited: status = %d", got)
	}

	// The probe has its own bucket, untouched by the traffic above.
	for attempt := 1; attempt <= cfg.Health.Burst; attempt++ {
		if got := server.do(t, http.MethodGet, healthPath, peer, nil).Code; got != http.StatusOK {
			t.Fatalf("health probe %d = %d, want %d", attempt, got, http.StatusOK)
		}
	}
	// And it is still bounded, so a flood of probes cannot run unmetered.
	if got := server.do(t, http.MethodGet, healthPath, peer, nil).Code; got != http.StatusTooManyRequests {
		t.Fatalf("health probe over its own burst = %d, want %d", got, http.StatusTooManyRequests)
	}

	// A starved probe must be distinguishable in the metric from ordinary
	// traffic being throttled, or the one alert that matters reads as noise.
	var sawHealth bool
	for _, entry := range server.observer.snapshot() {
		if entry.routeClass == string(RouteClassHealth) {
			sawHealth = true
			if entry.limiter != limiterHealth {
				t.Errorf("a health refusal was reported under limiter %q, want %q", entry.limiter, limiterHealth)
			}
		}
	}
	if !sawHealth {
		t.Error("no health refusal was recorded")
	}
}

// TestProxyTrustDecidesTheBucket is the reason the limiter reuses the address
// resolver instead of reading the header itself. Behind a trusted proxy each
// forwarded client gets its own bucket; from an untrusted peer the header is
// ignored, so a forged one cannot buy a fresh bucket per request.
func TestProxyTrustDecidesTheBucket(t *testing.T) {
	t.Parallel()

	t.Run("untrusted peer cannot choose its bucket", func(t *testing.T) {
		t.Parallel()
		clock := newTestClock()
		cfg := testRateLimitConfig(clock)
		cfg.Credential = ratelimit.Policy{Burst: 1000, Interval: time.Second}
		server := newRateLimitedServer(t, cfg, nil)

		const peer = "203.0.113.20:5000"
		for attempt := 0; attempt < cfg.Client.Burst; attempt++ {
			forged := map[string]string{apiKeyHeader: testAPIKey, forwardedForHeader: "10.1.1." + strconv.Itoa(attempt)}
			if got := server.do(t, http.MethodPost, "/v1/orders", peer, forged).Code; got != http.StatusCreated {
				t.Fatalf("request %d = %d, want %d", attempt+1, got, http.StatusCreated)
			}
		}
		forged := map[string]string{apiKeyHeader: testAPIKey, forwardedForHeader: "10.9.9.9"}
		if got := server.do(t, http.MethodPost, "/v1/orders", peer, forged).Code; got != http.StatusTooManyRequests {
			t.Fatalf("a forged X-Forwarded-For bought a new bucket: status = %d", got)
		}
	})

	t.Run("trusted proxy separates its clients", func(t *testing.T) {
		t.Parallel()
		clock := newTestClock()
		cfg := testRateLimitConfig(clock)
		cfg.Credential = ratelimit.Policy{Burst: 1000, Interval: time.Second}
		trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
		server := newRateLimitedServer(t, cfg, trusted)

		const proxy = "10.0.0.1:5000"
		first := map[string]string{apiKeyHeader: testAPIKey, forwardedForHeader: "198.51.100.7"}
		for attempt := 0; attempt < cfg.Client.Burst; attempt++ {
			if got := server.do(t, http.MethodPost, "/v1/orders", proxy, first).Code; got != http.StatusCreated {
				t.Fatalf("forwarded client request %d = %d, want %d", attempt+1, got, http.StatusCreated)
			}
		}
		if got := server.do(t, http.MethodPost, "/v1/orders", proxy, first).Code; got != http.StatusTooManyRequests {
			t.Fatalf("forwarded client was not limited: status = %d", got)
		}

		// A different client behind the same proxy is unaffected.
		second := map[string]string{apiKeyHeader: testAPIKey, forwardedForHeader: "198.51.100.8"}
		if got := server.do(t, http.MethodPost, "/v1/orders", proxy, second).Code; got != http.StatusCreated {
			t.Fatalf("a second forwarded client shared the first one's bucket: status = %d", got)
		}
	})
}

// TestMiddlewareRejectsBeforeParsingAuthenticationAndHandler proves the coarse
// limiter really is outermost. A body that would fail OpenAPI validation, sent
// with no credential at all, still gets 429 rather than 400 or 401 once the
// bucket is empty: nothing downstream of the limiter ran.
func TestMiddlewareRejectsBeforeParsingAuthenticationAndHandler(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	server := newRateLimitedServer(t, cfg, nil)

	const peer = "203.0.113.30:5000"
	// Spend the burst with requests that are themselves malformed and
	// unauthenticated, which proves those failures still cost a token.
	for attempt := 1; attempt <= cfg.Client.Burst; attempt++ {
		response := server.do(t, http.MethodPost, "/v1/orders", peer, nil)
		if response.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was refused inside the burst", attempt)
		}
	}

	refused := server.do(t, http.MethodPost, "/v1/orders", peer, nil)
	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: the limiter did not run before parsing and authentication",
			refused.Code, http.StatusTooManyRequests)
	}
	if created := server.orders.created.Load(); created != 0 {
		t.Fatalf("the use case ran %d times for unauthenticated requests, want 0", created)
	}
}

// TestCredentialLimiterRunsBeforeAuthenticationWithoutWeakeningIt pins the
// ordering of the two strict middlewares. The generated wrapper applies them
// in list order, so the last one listed is outermost, and the rate limiter is
// listed last on purpose: a valid credential over its limit is refused before
// the credential is compared again and before the handler runs.
//
// The second half is what makes that safe. An invalid credential still gets
// 401, because the fingerprinter refuses to name it and it therefore never
// reaches a bucket at all.
func TestCredentialLimiterRunsBeforeAuthenticationWithoutWeakeningIt(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	cfg.Client = ratelimit.Policy{Burst: 10000, Interval: time.Minute}
	cfg.Credential = ratelimit.Policy{Burst: 1, Interval: time.Minute}
	server := newRateLimitedServer(t, cfg, nil)

	const peer = "203.0.113.77:5000"
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusCreated {
		t.Fatalf("the first authenticated request = %d, want %d", got, http.StatusCreated)
	}
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code; got != http.StatusTooManyRequests {
		t.Fatalf("a valid credential over its limit = %d, want %d", got, http.StatusTooManyRequests)
	}
	// The limiter must not have become a way past authentication.
	invalid := authenticated("bogus-key-padded-to-be-long-enough")
	if got := server.do(t, http.MethodPost, "/v1/orders", peer, invalid).Code; got != http.StatusUnauthorized {
		t.Fatalf("an invalid credential = %d, want %d", got, http.StatusUnauthorized)
	}
}

// TestRejectionMetricCarriesOnlyBoundedAttributes checks the observability
// contract: a refusal is counted, and the attributes are the closed
// vocabularies of limiter and route class, never an address or a fingerprint.
func TestRejectionMetricCarriesOnlyBoundedAttributes(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	server := newRateLimitedServer(t, cfg, nil)

	// Exhaust the coarse limiter from several addresses and credentials, so
	// any per-identity attribute would show up as a distinct observation.
	for _, peer := range []string{"203.0.113.40:5000", "203.0.113.41:5000"} {
		for attempt := 0; attempt < cfg.Client.Burst+2; attempt++ {
			server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey))
		}
	}

	entries := server.observer.snapshot()
	if len(entries) == 0 {
		t.Fatal("no rejection was recorded")
	}
	allowedLimiters := map[string]struct{}{
		limiterClient: {}, limiterCredential: {}, limiterWebhook: {}, limiterHealth: {},
	}
	allowedClasses := map[string]struct{}{
		string(RouteClassHealth): {}, string(RouteClassDocs): {},
		string(RouteClassBusiness): {}, string(RouteClassOperations): {},
		string(RouteClassWebhook): {},
	}
	distinct := make(map[observation]struct{})
	for _, entry := range entries {
		if _, ok := allowedLimiters[entry.limiter]; !ok {
			t.Errorf("limiter attribute %q is outside the closed vocabulary", entry.limiter)
		}
		if _, ok := allowedClasses[entry.routeClass]; !ok {
			t.Errorf("route class attribute %q is outside the closed vocabulary", entry.routeClass)
		}
		distinct[entry] = struct{}{}
	}
	// Two addresses and one credential produced many rejections, but the
	// attribute space must not have grown with them.
	if len(distinct) > len(allowedLimiters)*len(allowedClasses) {
		t.Fatalf("rejections produced %d distinct attribute sets, more than the vocabulary allows", len(distinct))
	}
}

// TestConcurrentRequestsAreLimitedWithoutRaces runs the whole chain from many
// goroutines. Under -race it is the guard against a data race in the limiter,
// and it also proves the burst is honoured exactly rather than approximately
// when requests arrive at once.
func TestConcurrentRequestsAreLimitedWithoutRaces(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	cfg.Client = ratelimit.Policy{Burst: 50, Interval: time.Minute}
	cfg.Credential = ratelimit.Policy{Burst: 1000, Interval: time.Minute}
	server := newRateLimitedServer(t, cfg, nil)

	const peer = "203.0.113.50:5000"
	const requests = 200
	var allowed, refused atomic.Int64
	var group sync.WaitGroup
	for attempt := 0; attempt < requests; attempt++ {
		group.Add(1)
		go func() {
			defer group.Done()
			switch server.do(t, http.MethodPost, "/v1/orders", peer, authenticated(testAPIKey)).Code {
			case http.StatusTooManyRequests:
				refused.Add(1)
			default:
				allowed.Add(1)
			}
		}()
	}
	group.Wait()

	// The clock never moved, so exactly the burst may pass: no more, because
	// that would be a lost update, and no fewer, because that would be one.
	if got := allowed.Load(); got != int64(cfg.Client.Burst) {
		t.Fatalf("allowed %d concurrent requests, want exactly %d", got, cfg.Client.Burst)
	}
	if got := refused.Load(); got != requests-int64(cfg.Client.Burst) {
		t.Fatalf("refused %d requests, want %d", got, requests-int64(cfg.Client.Burst))
	}
}

// TestDisabledConfigurationBuildsNoLimiter proves the opt-out is a real one:
// with limiting off, no request is ever refused for rate.
func TestDisabledConfigurationBuildsNoLimiter(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	cfg := testRateLimitConfig(clock)
	cfg.Enabled = false
	server := newRateLimitedServer(t, cfg, nil)

	for attempt := 0; attempt < 100; attempt++ {
		if got := server.do(t, http.MethodPost, "/v1/orders", "203.0.113.60:5000", authenticated(testAPIKey)).Code; got == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited while limiting is disabled", attempt+1)
		}
	}
}

func TestEnabledConfigurationRequiresCredentialFingerprinter(t *testing.T) {
	t.Parallel()

	cfg := testRateLimitConfig(newTestClock())
	if _, err := newRateLimiters(cfg, nil, nil); err == nil {
		t.Fatal("enabled rate limiting accepted a nil credential fingerprinter")
	}
}

// newRateLimitedTestHandler builds the real server with every bucket already
// spent, so the next request of any operation is refused. The contract cases
// use it to prove the declared 429 against the middleware chain the process
// runs, rather than against a stub that merely returns the status.
//
// A burst of one, spent once per limiter, is the whole trick: the coarse
// limiter refuses anything that reaches it, and the webhook and credential
// buckets are drained too so an operation that would pass the coarse gate
// still meets an empty bucket.
func newRateLimitedTestHandler() http.Handler {
	clock := newTestClock()
	spent := ratelimit.Policy{Burst: 1, Interval: time.Hour}
	cfg := RateLimitConfig{
		Enabled:            true,
		Client:             spent,
		ClientCapacity:     8,
		Credential:         spent,
		CredentialCapacity: 8,
		Webhook:            spent,
		Health:             spent,
		HealthCapacity:     8,
		IdleTTL:            time.Hour,
		Clock:              clock.Now,
	}
	verifier, err := auth.NewAPIKeyVerifier([]string{testAPIKey, secondTestAPIKey})
	if err != nil {
		panic("build verifier: " + err.Error())
	}
	limiters, err := newRateLimiters(cfg, verifier, nil)
	if err != nil {
		panic("build limiters: " + err.Error())
	}
	// Drain each bucket the contract cases can reach. The contract requests
	// arrive from httptest's default peer, so that is the address to spend.
	defaultPeer := netip.MustParseAddr("192.0.2.1")
	limiters.allowClient(defaultPeer, RouteClassBusiness)
	limiters.allowClient(defaultPeer, RouteClassHealth)
	limiters.allowWebhook()
	limiters.allowCredential(testAPIKey, RouteClassBusiness)

	mux := http.NewServeMux()
	api := NewAPIHandler(
		health.New("payments-test", "test", nil),
		&countingOrders{}, nil, concurrentWebhooks{}, nil)
	openapi.HandlerWithOptions(
		newStrictHandler(zap.NewNop(), api, verifier, limiters),
		openapi.StdHTTPServerOptions{BaseRouter: mux, ErrorHandlerFunc: requestErrorHandler})
	return newServer(Config{Address: ":0", ShutdownTimeout: time.Second}, zap.NewNop(), mux, limiters).server.Handler
}

// concurrentWebhooks accepts every event and keeps no state, so it is safe to
// call from many goroutines. The webhook use case itself is tested elsewhere;
// here it only has to not be the thing that fails.
type concurrentWebhooks struct{}

func (concurrentWebhooks) Receive(context.Context, webhookapp.ReceiveInput) (webhookapp.Outcome, error) {
	return webhookapp.OutcomeAccepted, nil
}

func mustVerifier(t *testing.T) *auth.APIKeyVerifier {
	t.Helper()
	verifier, err := auth.NewAPIKeyVerifier([]string{testAPIKey, secondTestAPIKey})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	return verifier
}
