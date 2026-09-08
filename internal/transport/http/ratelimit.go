package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/platform/ratelimit"
	"github.com/rmotti/payments-boilerplate/internal/transport/http/openapi"
)

// retryAfterHeader tells a refused client when a token will be available. It
// is expressed in whole seconds, the delta form of RFC 9110.
const retryAfterHeader = "Retry-After"

// RouteClass groups operations by the rate limiting policy that applies to
// them. Classes exist instead of paths so the metric's series count is fixed
// by this list rather than by how many routes the contract grows.
type RouteClass string

// The classes the policy distinguishes.
const (
	// RouteClassHealth is the liveness and readiness probe. It gets its own
	// generous allowance so ordinary traffic, which is counted separately,
	// cannot make a deployment look unhealthy by exhausting a shared bucket.
	RouteClassHealth RouteClass = "health"
	// RouteClassDocs is Swagger UI and the OpenAPI document. A page load
	// fetches several assets, so the allowance is the coarse client one; the
	// routes are opt-in and usually absent entirely.
	RouteClassDocs RouteClass = "docs"
	// RouteClassBusiness is order creation, checkout and order reads: the
	// operations that cost money to serve and that a credential owns.
	RouteClassBusiness RouteClass = "business"
	// RouteClassOperations is webhook-event inspection and reprocessing.
	// Reprocessing re-enqueues work, so it is limited like a business write.
	RouteClassOperations RouteClass = "operations"
	// RouteClassWebhook is the provider endpoint. Its caller is Stripe, whose
	// address is not an identity we can trust or predict, so it is limited by
	// a single global bucket rather than per client.
	RouteClassWebhook RouteClass = "webhook"
)

// The limiter names a refusal is reported under. They are a closed vocabulary,
// mirrored by the metrics package's allowlist, so the series count stays fixed.
const (
	limiterClient     = "client"
	limiterCredential = "credential"
	limiterWebhook    = "webhook"
	limiterHealth     = "health"
)

// RateLimitObserver records refusals. It is the transport's own vocabulary:
// the implementation in the metrics package turns it into a counter, and both
// arguments are closed vocabularies rather than anything a client chose.
type RateLimitObserver interface {
	Rejected(limiter, routeClass string)
}

// CredentialFingerprinter turns a valid credential into a stable opaque
// identifier, and refuses to produce one for a credential that is not
// configured. That refusal is what keeps an unauthenticated caller from
// creating a bucket per invented header value.
type CredentialFingerprinter interface {
	Fingerprint(candidate string) (string, bool)
}

// RateLimitConfig is the tunable part of the policy. Every field is validated
// before a server is built, and again by configuration loading, so a bad value
// stops the process at startup rather than at the first request.
type RateLimitConfig struct {
	// Enabled turns the whole feature off. It exists for a deployment that
	// terminates rate limiting at its edge and wants one place to disable
	// this one, not as a convenience for skipping the limits.
	Enabled bool

	// Client is the coarse limit applied per resolved client address to routes
	// other than webhook and health, before parsing, authentication and any
	// handler. Those two public routes have dedicated pre-routing limits.
	Client ratelimit.Policy
	// ClientCapacity bounds how many client buckets are held at once.
	ClientCapacity int

	// Credential is the limit applied per valid credential fingerprint, on
	// top of the coarse one, for authenticated operations.
	Credential ratelimit.Policy
	// CredentialCapacity bounds how many credential buckets are held. It is
	// small because the number of active integration keys is small.
	CredentialCapacity int

	// Webhook is the single global bucket the provider endpoint shares. It runs
	// before the request body or signature is read.
	Webhook ratelimit.Policy

	// Health is the coarse limit applied per client address to the health
	// probe, kept separate so probes and traffic cannot starve each other.
	Health ratelimit.Policy
	// HealthCapacity bounds how many health buckets are held.
	HealthCapacity int

	// IdleTTL reclaims a bucket untouched for this long, so a limiter shrinks
	// during quiet periods instead of staying at its high-water mark.
	IdleTTL time.Duration

	// Clock is injected by tests to prove refill without sleeping. Nil means
	// time.Now.
	Clock ratelimit.Clock
}

// rateLimiters holds the four limiters the policy uses. A nil *rateLimiters
// means limiting is disabled, and every method on it allows.
type rateLimiters struct {
	client     *ratelimit.Limiter
	credential *ratelimit.Limiter
	webhook    *ratelimit.Limiter
	health     *ratelimit.Limiter

	fingerprinter CredentialFingerprinter
	observer      RateLimitObserver
}

// newRateLimiters builds the limiters, or nil when the feature is disabled.
func newRateLimiters(
	cfg RateLimitConfig,
	fingerprinter CredentialFingerprinter,
	observer RateLimitObserver,
) (*rateLimiters, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if fingerprinter == nil {
		return nil, fmt.Errorf("rate limiting requires a credential fingerprinter")
	}
	options := func(capacity int) ratelimit.Options {
		return ratelimit.Options{Capacity: capacity, TTL: cfg.IdleTTL, Clock: cfg.Clock}
	}
	client, err := ratelimit.New(cfg.Client, options(cfg.ClientCapacity), "RATE_LIMIT_CLIENT")
	if err != nil {
		return nil, err
	}
	credential, err := ratelimit.New(cfg.Credential, options(cfg.CredentialCapacity), "RATE_LIMIT_CREDENTIAL")
	if err != nil {
		return nil, err
	}
	// The webhook bucket is global, so one key is all it ever holds.
	webhook, err := ratelimit.New(cfg.Webhook, options(1), "RATE_LIMIT_WEBHOOK")
	if err != nil {
		return nil, err
	}
	health, err := ratelimit.New(cfg.Health, options(cfg.HealthCapacity), "RATE_LIMIT_HEALTH")
	if err != nil {
		return nil, err
	}
	return &rateLimiters{
		client:        client,
		credential:    credential,
		webhook:       webhook,
		health:        health,
		fingerprinter: fingerprinter,
		observer:      observer,
	}, nil
}

// globalWebhookKey is the one key the webhook limiter ever holds. The provider
// endpoint is limited as a whole because the caller is Stripe: its source
// addresses are neither stable nor ours to treat as an identity, and counting
// them per address would let a forged peer dodge the limit while a legitimate
// redelivery burst from a new address gets refused.
const globalWebhookKey = "webhook"

// allowClient spends one token from the coarse per-address bucket. It is the
// only limiter that runs before the request is parsed or authenticated, so its
// key is the address the ClientAddressResolver already settled on and nothing
// the client wrote.
func (l *rateLimiters) allowClient(address netip.Addr, class RouteClass) (ratelimit.Decision, bool) {
	if l == nil {
		return ratelimit.Decision{Allowed: true}, true
	}
	// The probe counts against its own bucket, and its refusals are reported
	// under their own limiter value, so a starved probe is distinguishable
	// from ordinary traffic being throttled.
	limiter, name := l.client, limiterClient
	if class == RouteClassHealth {
		limiter, name = l.health, limiterHealth
	}
	// Group by the prefix a single host controls, so an IPv6 client cannot
	// spread its requests across the addresses of its own /64.
	decision := limiter.Allow(ratelimit.Key(ClientBucket(address)))
	if !decision.Allowed {
		l.observe(name, class)
	}
	return decision, decision.Allowed
}

// allowWebhook spends one token from the global provider bucket.
func (l *rateLimiters) allowWebhook() (ratelimit.Decision, bool) {
	if l == nil {
		return ratelimit.Decision{Allowed: true}, true
	}
	decision := l.webhook.Allow(globalWebhookKey)
	if !decision.Allowed {
		l.observe(limiterWebhook, RouteClassWebhook)
	}
	return decision, decision.Allowed
}

// allowCredential spends one token from the bucket of a valid credential.
//
// It returns allowed for a credential it cannot fingerprint. That is not a
// gap: an invalid credential never reaches an authenticated operation, and
// giving it a bucket would let anyone mint unbounded keys from a header. Such
// a request stays accounted for by the coarse address limiter alone.
func (l *rateLimiters) allowCredential(credential string, class RouteClass) (ratelimit.Decision, bool) {
	if l == nil || l.fingerprinter == nil {
		return ratelimit.Decision{Allowed: true}, true
	}
	fingerprint, ok := l.fingerprinter.Fingerprint(credential)
	if !ok {
		return ratelimit.Decision{Allowed: true}, true
	}
	decision := l.credential.Allow(fingerprint)
	if !decision.Allowed {
		l.observe(limiterCredential, class)
	}
	return decision, decision.Allowed
}

func (l *rateLimiters) observe(limiter string, class RouteClass) {
	if l.observer == nil {
		return
	}
	l.observer.Rejected(limiter, string(class))
}

// clientRateLimitMiddleware is the pre-routing gate, and it sits outside body
// limiting, OpenAPI decoding and authentication on purpose. A request refused
// here costs the process one map lookup: nothing has been parsed, no credential
// compared and no handler entered. Putting it after any of those would mean an
// unauthenticated flood still paid for all of them.
//
// The class is derived from the path rather than from an operation id, since no
// routing has happened yet. Health uses its own address bucket. The webhook is
// deliberately different: it spends the provider's global bucket here, before
// its body or signature is read, and never treats a Stripe source IP as the
// provider's identity.
func clientRateLimitMiddleware(limiters *rateLimiters, next http.Handler) http.Handler {
	if limiters == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := coarseRouteClass(r.URL.Path)
		// The webhook bucket is global and therefore does not depend on a
		// network address being available. In particular, a local Unix-socket
		// peer must not accidentally bypass the provider-wide limit.
		if class == RouteClassWebhook {
			decision, allowed := limiters.allowWebhook()
			if !allowed {
				writeRateLimited(w, r, decision.RetryAfter)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		address, ok := ClientAddressFromContext(r.Context())
		if !ok {
			// No IP address to count, which happens only for a peer on a Unix
			// socket. Such a connection is local by construction and is not
			// the traffic this limiter defends against.
			next.ServeHTTP(w, r)
			return
		}
		decision, allowed := limiters.allowClient(address, class)
		if !allowed {
			writeRateLimited(w, r, decision.RetryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// coarseRouteClass classifies a path before routing. It recognizes only what
// the coarse limiter needs to tell apart; the precise class of a business
// operation is known later, from the operation id.
func coarseRouteClass(path string) RouteClass {
	switch {
	case path == healthPath:
		return RouteClassHealth
	case path == webhookPath:
		return RouteClassWebhook
	case path == docsPath || path == openAPIPath || strings.HasPrefix(path, docsIndexPath):
		return RouteClassDocs
	default:
		return RouteClassBusiness
	}
}

// operationRateLimitMiddleware applies the per-operation limits, inside the
// strict server where the operation id is known.
//
// It runs before the authentication middleware in the chain below, but reads
// the credential only through the fingerprinter, which answers for valid keys
// alone. An invalid key is therefore refused by authentication as it always
// was, and never gets a bucket of its own here.
func operationRateLimitMiddleware(limiters *rateLimiters) openapi.StrictMiddlewareFunc {
	return func(next openapi.StrictHandlerFunc, operationID string) openapi.StrictHandlerFunc {
		if limiters == nil {
			return next
		}
		class := routeClassFor(operationID)
		return func(
			ctx context.Context,
			w http.ResponseWriter,
			r *http.Request,
			request any,
		) (any, error) {
			switch class {
			case RouteClassWebhook, RouteClassHealth, RouteClassDocs:
				// Webhook and health were already limited by the pre-routing gate;
				// documentation never reaches the strict server.
			case RouteClassBusiness, RouteClassOperations:
				credential := r.Header.Get(apiKeyHeader)
				if decision, allowed := limiters.allowCredential(credential, class); !allowed {
					return nil, rateLimitedError{retryAfter: decision.RetryAfter}
				}
			}
			return next(ctx, w, r, request)
		}
	}
}

// routeClassFor maps an operation id onto its class. Operations are listed
// explicitly, so one added to the contract lands in the default and is limited
// like a business operation until someone decides otherwise, rather than
// silently escaping the policy.
func routeClassFor(operationID string) RouteClass {
	switch operationID {
	case "GetHealth":
		return RouteClassHealth
	case "ReceiveStripeWebhook":
		return RouteClassWebhook
	case "ListWebhookEvents", "ReprocessWebhookEvent":
		return RouteClassOperations
	default:
		return RouteClassBusiness
	}
}

// rateLimitedError carries a refusal out of the strict middleware, where the
// only way to answer is to return an error. The response error handler turns
// it into the 429 below.
type rateLimitedError struct{ retryAfter time.Duration }

func (rateLimitedError) Error() string { return "too many requests" }

// writeRateLimited answers a refused request with the project's public error
// envelope, so a 429 looks like every other error: the correlation id, the
// strict security headers and a stable code. Retry-After is added because a
// client that is being limited needs to know when to come back, and it is the
// one piece of information the refusal is allowed to disclose.
//
// Nothing about the identity that was limited appears in the body. Which
// bucket ran out is an internal detail; telling a caller whether it was
// refused by its address or by its credential would confirm the credential is
// valid to someone who guessed one.
func writeRateLimited(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	if retryAfter <= 0 {
		retryAfter = time.Second
	}
	w.Header().Set(retryAfterHeader, strconv.Itoa(int(retryAfter/time.Second)))
	writeError(w, r, http.StatusTooManyRequests, codeRateLimited, "too many requests")
}
