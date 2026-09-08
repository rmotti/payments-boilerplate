// Package config loads and validates process configuration.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	outboxapp "github.com/rmotti/payments-boilerplate/internal/application/outbox"
)

// PublishAttemptBudget is the ceiling one publish attempt may take, including
// redialing the broker. It lives here, rather than being imported from the
// adapter, because configuration must not depend on adapters; a test asserts
// the two stay equal.
const PublishAttemptBudget = 5 * time.Second

// EnvironmentDevelopment is the only APP_ENV in which developer conveniences,
// such as public API documentation, are allowed without further protection.
const EnvironmentDevelopment = "development"

// Config contains all process configuration loaded from environment variables.
type Config struct {
	ServiceName string

	Environment string `env:"APP_ENV" envDefault:"development"`
	HTTPAddress string `env:"HTTP_ADDRESS"`
	Port        string `env:"PORT"`

	DatabaseURL                   string        `env:"DATABASE_URL,required"`
	DatabaseMaxOpenConnections    int           `env:"DATABASE_MAX_OPEN_CONNECTIONS" envDefault:"10"`
	DatabaseMaxIdleConnections    int           `env:"DATABASE_MAX_IDLE_CONNECTIONS" envDefault:"5"`
	DatabaseConnectionMaxLifetime time.Duration `env:"DATABASE_CONNECTION_MAX_LIFETIME" envDefault:"30m"`

	RabbitMQURL string `env:"RABBITMQ_URL"`

	// Relay tuning. The defaults suit a single worker on a small deployment;
	// they exist as configuration because the right values depend on volume.
	OutboxBatchSize int           `env:"OUTBOX_BATCH_SIZE" envDefault:"20"`
	OutboxInterval  time.Duration `env:"OUTBOX_INTERVAL" envDefault:"1s"`
	// OutboxLeaseDuration must comfortably exceed the time to publish a whole
	// batch: a lease that expires mid-publish lets another instance take the
	// same message, which is safe but wasteful.
	OutboxLeaseDuration time.Duration `env:"OUTBOX_LEASE_DURATION" envDefault:"2m"`
	OutboxBackoffBase   time.Duration `env:"OUTBOX_BACKOFF_BASE" envDefault:"2s"`
	OutboxBackoffMax    time.Duration `env:"OUTBOX_BACKOFF_MAX" envDefault:"5m"`
	// OutboxAlertAfterAttempts only controls when a struggling message is
	// reported. It never stops the retries: only a permanent failure does.
	OutboxAlertAfterAttempts int `env:"OUTBOX_ALERT_AFTER_ATTEMPTS" envDefault:"10"`

	// Consumer tuning. Prefetch bounds the messages in flight; concurrency
	// bounds how many are applied at once. The retry delays are the ladder a
	// transient failure walks, and the last one repeats until the attempt
	// budget is spent.
	ConsumerConcurrency int             `env:"CONSUMER_CONCURRENCY" envDefault:"4"`
	ConsumerPrefetch    int             `env:"CONSUMER_PREFETCH" envDefault:"4"`
	ConsumerMaxAttempts int             `env:"CONSUMER_MAX_ATTEMPTS" envDefault:"10"`
	ConsumerRetryDelays []time.Duration `env:"CONSUMER_RETRY_DELAYS" envDefault:"5s,30s,2m,10m"`

	IntegrationAPIKeys []string `env:"INTEGRATION_API_KEYS"`

	// DocsEnabled opts in to serving Swagger UI and the OpenAPI document. The
	// default is off in every environment: development turns it on explicitly
	// through .env.example and the Compose file. Outside development the
	// routes additionally require a valid X-API-Key. See ADR 0016.
	DocsEnabled bool `env:"DOCS_ENABLED" envDefault:"false"`

	// TrustedProxyCIDRs lists the networks whose X-Forwarded-For header is
	// believed. Empty means no proxy is trusted and the TCP peer is the
	// client, which is the safe default for a process reached directly.
	TrustedProxyCIDRs []string `env:"TRUSTED_PROXY_CIDRS"`
	// TrustedProxies is the parsed, validated form of TrustedProxyCIDRs.
	TrustedProxies []netip.Prefix

	// Rate limiting. The limits are per process, so a deployment with N
	// replicas behind a balancer admits up to N times these numbers; ADR 0018
	// records why that is acceptable for this shape of deployment and what to
	// do when it is not.
	//
	// Each limit is a burst plus the interval in which the whole burst
	// refills, so the sustained rate is burst per interval. The defaults are
	// deliberately generous: a limit that trips on a legitimate integrator is
	// worse for this project than one that lets an abusive client through a
	// little longer.
	RateLimitEnabled bool `env:"RATE_LIMIT_ENABLED" envDefault:"true"`

	// The coarse limit, applied per resolved client address before parsing,
	// authentication and any handler except webhook and health, which have
	// dedicated pre-routing limits. It is intentionally wider than the
	// credential limit so the latter remains the effective business quota.
	RateLimitClientBurst    int           `env:"RATE_LIMIT_CLIENT_BURST" envDefault:"1200"`
	RateLimitClientInterval time.Duration `env:"RATE_LIMIT_CLIENT_INTERVAL" envDefault:"1m"`
	// RateLimitClientCapacity bounds how many address buckets exist at once.
	// It is the memory ceiling of the coarse limiter and, at the default, is
	// a few hundred kilobytes.
	RateLimitClientCapacity int `env:"RATE_LIMIT_CLIENT_CAPACITY" envDefault:"10000"`

	// The per-credential limit, applied on top of the coarse one to
	// authenticated operations, keyed by a fingerprint of the credential.
	RateLimitCredentialBurst    int           `env:"RATE_LIMIT_CREDENTIAL_BURST" envDefault:"600"`
	RateLimitCredentialInterval time.Duration `env:"RATE_LIMIT_CREDENTIAL_INTERVAL" envDefault:"1m"`
	// RateLimitCredentialCapacity is small because the number of active
	// integration keys is small; it exists so the map is bounded like the
	// others rather than because it is expected to bind.
	RateLimitCredentialCapacity int `env:"RATE_LIMIT_CREDENTIAL_CAPACITY" envDefault:"64"`

	// The global provider bucket. It is generous because a legitimate Stripe
	// backlog redelivers in bursts, and refusing those costs event latency.
	RateLimitWebhookBurst    int           `env:"RATE_LIMIT_WEBHOOK_BURST" envDefault:"600"`
	RateLimitWebhookInterval time.Duration `env:"RATE_LIMIT_WEBHOOK_INTERVAL" envDefault:"1m"`

	// The health probe limit, per address and separate from the coarse one so
	// ordinary traffic cannot make a deployment look unhealthy.
	RateLimitHealthBurst    int           `env:"RATE_LIMIT_HEALTH_BURST" envDefault:"120"`
	RateLimitHealthInterval time.Duration `env:"RATE_LIMIT_HEALTH_INTERVAL" envDefault:"1m"`
	RateLimitHealthCapacity int           `env:"RATE_LIMIT_HEALTH_CAPACITY" envDefault:"1000"`

	// RateLimitIdleTTL reclaims a bucket nobody has touched for this long, so
	// the limiters shrink during quiet periods instead of holding their
	// high-water mark until the process restarts.
	RateLimitIdleTTL time.Duration `env:"RATE_LIMIT_IDLE_TTL" envDefault:"10m"`

	StripeSecretKey     string `env:"STRIPE_SECRET_KEY"`
	StripeWebhookSecret string `env:"STRIPE_WEBHOOK_SECRET"`
	StripeSuccessURL    string `env:"STRIPE_SUCCESS_URL"`
	StripeCancelURL     string `env:"STRIPE_CANCEL_URL"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	StartupTimeout  time.Duration `env:"STARTUP_TIMEOUT" envDefault:"30s"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`

	OTelEnabled          bool          `env:"OTEL_ENABLED" envDefault:"false"`
	OTelServiceName      string        `env:"OTEL_SERVICE_NAME"`
	OTelExporterEndpoint string        `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"http://localhost:4318"`
	OTelExportInterval   time.Duration `env:"OTEL_EXPORT_INTERVAL" envDefault:"10s"`

	// Background metric sampling. The samplers read backlog from PostgreSQL
	// and queue depth from the broker on their own goroutines, so the interval
	// is a load decision rather than a latency one: the timeout bounds one
	// round and must stay below the interval, or a stalled dependency would
	// make rounds overlap.
	MetricsSampleInterval time.Duration `env:"METRICS_SAMPLE_INTERVAL" envDefault:"15s"`
	MetricsSampleTimeout  time.Duration `env:"METRICS_SAMPLE_TIMEOUT" envDefault:"5s"`

	MigrationsDir string `env:"MIGRATIONS_DIR" envDefault:"db/migrations"`
}

// Load reads an optional local .env file and then parses process environment.
// Existing environment variables always take precedence over the file.
func Load(serviceName, defaultHTTPAddress string, requireRabbitMQ bool) (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}

	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parse environment: %w", err)
	}

	cfg.ServiceName = serviceName
	if cfg.OTelServiceName == "" {
		cfg.OTelServiceName = serviceName
	}
	if cfg.Port != "" {
		if strings.Contains(cfg.Port, ":") {
			return Config{}, errors.New("PORT must contain only a port number")
		}
		cfg.HTTPAddress = ":" + cfg.Port
	}
	if cfg.HTTPAddress == "" {
		cfg.HTTPAddress = defaultHTTPAddress
	}

	if requireRabbitMQ && cfg.RabbitMQURL == "" {
		return Config{}, errors.New("RABBITMQ_URL is required for this process")
	}
	if cfg.DatabaseMaxOpenConnections < 1 {
		return Config{}, errors.New("DATABASE_MAX_OPEN_CONNECTIONS must be positive")
	}
	if cfg.DatabaseMaxIdleConnections < 0 || cfg.DatabaseMaxIdleConnections > cfg.DatabaseMaxOpenConnections {
		return Config{}, errors.New("DATABASE_MAX_IDLE_CONNECTIONS must be between zero and the maximum open connections")
	}
	if cfg.StartupTimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		return Config{}, errors.New("startup and shutdown timeouts must be positive")
	}
	if cfg.OutboxBatchSize < 1 {
		return Config{}, errors.New("OUTBOX_BATCH_SIZE must be positive")
	}
	if cfg.OutboxInterval <= 0 {
		return Config{}, errors.New("OUTBOX_INTERVAL must be positive")
	}
	if cfg.OutboxAlertAfterAttempts < 1 {
		return Config{}, errors.New("OUTBOX_ALERT_AFTER_ATTEMPTS must be positive")
	}
	if cfg.OutboxBackoffBase <= 0 || cfg.OutboxBackoffMax <= 0 {
		return Config{}, errors.New("outbox backoff durations must be positive")
	}
	if cfg.OutboxBackoffMax < cfg.OutboxBackoffBase {
		return Config{}, errors.New("OUTBOX_BACKOFF_MAX must not be smaller than OUTBOX_BACKOFF_BASE")
	}
	// A lease shorter than the worst-case time to publish a whole batch would
	// expire while the relay is still working through it. The publisher bounds
	// each attempt. A final reserve is left for recording the outcome in
	// PostgreSQL after the publication window closes.
	minimumLease := time.Duration(cfg.OutboxBatchSize)*PublishAttemptBudget + outboxapp.SettlementReserve
	if cfg.OutboxLeaseDuration < minimumLease {
		return Config{}, fmt.Errorf(
			"OUTBOX_LEASE_DURATION must be at least OUTBOX_BATCH_SIZE x %s + %s settlement reserve (%s)",
			PublishAttemptBudget, outboxapp.SettlementReserve, minimumLease)
	}
	if cfg.ConsumerConcurrency < 1 {
		return Config{}, errors.New("CONSUMER_CONCURRENCY must be positive")
	}
	// A prefetch below the concurrency starves workers: the broker refuses to
	// deliver more unacknowledged messages than the prefetch allows, so the
	// extra goroutines would sit idle forever.
	if cfg.ConsumerPrefetch < cfg.ConsumerConcurrency {
		return Config{}, errors.New("CONSUMER_PREFETCH must be at least CONSUMER_CONCURRENCY")
	}
	// A budget of one would dead-letter on the first transient failure,
	// defeating the retry ladder entirely.
	if cfg.ConsumerMaxAttempts < 2 {
		return Config{}, errors.New("CONSUMER_MAX_ATTEMPTS must be greater than one")
	}
	if len(cfg.ConsumerRetryDelays) == 0 {
		return Config{}, errors.New("CONSUMER_RETRY_DELAYS must list at least one delay")
	}
	previous := time.Duration(0)
	for _, delay := range cfg.ConsumerRetryDelays {
		if delay <= 0 {
			return Config{}, errors.New("CONSUMER_RETRY_DELAYS must contain only positive delays")
		}
		// Ascending order is what makes the ladder a backoff. Out of order it
		// would still work, but it would no longer be one.
		if delay <= previous {
			return Config{}, errors.New("CONSUMER_RETRY_DELAYS must be strictly ascending")
		}
		previous = delay
	}
	// The worker runs the relay and the consumer over one pool. Each consumer
	// worker holds a connection for its whole transaction, so a pool that
	// cannot cover them plus the relay and the health check deadlocks under
	// load rather than merely slowing down.
	const relayAndHealthConnections = 2
	if requireRabbitMQ && cfg.DatabaseMaxOpenConnections < cfg.ConsumerConcurrency+relayAndHealthConnections {
		return Config{}, fmt.Errorf(
			"DATABASE_MAX_OPEN_CONNECTIONS must be at least CONSUMER_CONCURRENCY plus %d for the relay and health checks (%d)",
			relayAndHealthConnections, cfg.ConsumerConcurrency+relayAndHealthConnections)
	}
	if cfg.MetricsSampleInterval <= 0 || cfg.MetricsSampleTimeout <= 0 {
		return Config{}, errors.New("metric sampling interval and timeout must be positive")
	}
	// A timeout at or above the interval lets one slow round still be running
	// when the next fires, which turns a stalled dependency into unbounded
	// concurrent sampling instead of a visible gap.
	if cfg.MetricsSampleTimeout >= cfg.MetricsSampleInterval {
		return Config{}, errors.New("METRICS_SAMPLE_TIMEOUT must be shorter than METRICS_SAMPLE_INTERVAL")
	}
	if cfg.OTelEnabled && cfg.OTelExporterEndpoint == "" {
		return Config{}, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT is required when telemetry is enabled")
	}
	if err := cfg.validateRateLimits(); err != nil {
		return Config{}, err
	}
	trustedProxies, err := ParseTrustedProxies(cfg.TrustedProxyCIDRs)
	if err != nil {
		return Config{}, err
	}
	cfg.TrustedProxies = trustedProxies

	return cfg, nil
}

// RateLimitPolicy is the validated rate limiting configuration in the shape
// the transport layer consumes. It is a plain struct rather than the
// transport's own type so that configuration keeps depending on nothing.
type RateLimitPolicy struct {
	Enabled bool

	ClientBurst    int
	ClientInterval time.Duration
	ClientCapacity int

	CredentialBurst    int
	CredentialInterval time.Duration
	CredentialCapacity int

	WebhookBurst    int
	WebhookInterval time.Duration

	HealthBurst    int
	HealthInterval time.Duration
	HealthCapacity int

	IdleTTL time.Duration
}

// RateLimitPolicy groups the rate limiting values Load has already validated.
func (c Config) RateLimitPolicy() RateLimitPolicy {
	return RateLimitPolicy{
		Enabled:            c.RateLimitEnabled,
		ClientBurst:        c.RateLimitClientBurst,
		ClientInterval:     c.RateLimitClientInterval,
		ClientCapacity:     c.RateLimitClientCapacity,
		CredentialBurst:    c.RateLimitCredentialBurst,
		CredentialInterval: c.RateLimitCredentialInterval,
		CredentialCapacity: c.RateLimitCredentialCapacity,
		WebhookBurst:       c.RateLimitWebhookBurst,
		WebhookInterval:    c.RateLimitWebhookInterval,
		HealthBurst:        c.RateLimitHealthBurst,
		HealthInterval:     c.RateLimitHealthInterval,
		HealthCapacity:     c.RateLimitHealthCapacity,
		IdleTTL:            c.RateLimitIdleTTL,
	}
}

// validateRateLimits checks every rate limiting value, whether or not the
// feature is enabled. A deployment that turns limiting on later should find
// out about a typo at the first startup after the edit, not at the one that
// enables it.
func (c Config) validateRateLimits() error {
	limits := []struct {
		name     string
		burst    int
		interval time.Duration
	}{
		{"RATE_LIMIT_CLIENT", c.RateLimitClientBurst, c.RateLimitClientInterval},
		{"RATE_LIMIT_CREDENTIAL", c.RateLimitCredentialBurst, c.RateLimitCredentialInterval},
		{"RATE_LIMIT_WEBHOOK", c.RateLimitWebhookBurst, c.RateLimitWebhookInterval},
		{"RATE_LIMIT_HEALTH", c.RateLimitHealthBurst, c.RateLimitHealthInterval},
	}
	for _, limit := range limits {
		if limit.burst < 1 {
			return fmt.Errorf("%s_BURST must be at least 1", limit.name)
		}
		if limit.interval <= 0 {
			return fmt.Errorf("%s_INTERVAL must be positive", limit.name)
		}
		// One token must still be a duration a client can be told to wait.
		if limit.interval/time.Duration(limit.burst) <= 0 {
			return fmt.Errorf("%s_INTERVAL is too short to refill %s_BURST tokens", limit.name, limit.name)
		}
	}

	capacities := []struct {
		name  string
		value int
	}{
		{"RATE_LIMIT_CLIENT_CAPACITY", c.RateLimitClientCapacity},
		{"RATE_LIMIT_CREDENTIAL_CAPACITY", c.RateLimitCredentialCapacity},
		{"RATE_LIMIT_HEALTH_CAPACITY", c.RateLimitHealthCapacity},
	}
	for _, capacity := range capacities {
		if capacity.value < 1 {
			return fmt.Errorf("%s must be at least 1", capacity.name)
		}
	}

	if c.RateLimitIdleTTL < 0 {
		return errors.New("RATE_LIMIT_IDLE_TTL must not be negative")
	}
	// A TTL shorter than the window a bucket refills in would reclaim buckets
	// that are still spending, handing their keys a fresh burst and making the
	// sustained rate unenforceable.
	if c.RateLimitIdleTTL > 0 {
		for _, limit := range limits {
			if c.RateLimitIdleTTL < limit.interval {
				return fmt.Errorf(
					"RATE_LIMIT_IDLE_TTL must not be shorter than %s_INTERVAL, or a bucket would be reclaimed while still refilling",
					limit.name)
			}
		}
	}
	return nil
}

// IsDevelopment reports whether the process runs under the development
// environment, the only one where documentation may be served without a key.
func (c Config) IsDevelopment() bool {
	return c.Environment == EnvironmentDevelopment
}

// ParseTrustedProxies validates TRUSTED_PROXY_CIDRS. Every entry must be a
// CIDR block; a bare address is rejected so a typo such as "10.0.0.1" cannot
// silently trust a whole network or nothing at all. Blank entries left by a
// trailing comma are ignored.
func ParseTrustedProxies(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS entry %q must be a CIDR block such as 10.0.0.0/8", value)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	return prefixes, nil
}

// ValidateStripe checks every Stripe dependency used by the API. It is kept as
// the aggregate validation for callers that use both real adapters.
func (c Config) ValidateStripe() error {
	if err := c.ValidateStripeCheckout(); err != nil {
		return err
	}
	return c.ValidateStripeWebhook()
}

// ValidateStripeCheckout checks the configuration required by the hosted
// checkout adapter. It is separate from webhook validation because tests may
// replace either application port independently.
func (c Config) ValidateStripeCheckout() error {
	if c.StripeSecretKey == "" {
		return errors.New("STRIPE_SECRET_KEY is required for the API")
	}
	if err := validateReturnURL("STRIPE_SUCCESS_URL", c.StripeSuccessURL); err != nil {
		return err
	}
	return validateReturnURL("STRIPE_CANCEL_URL", c.StripeCancelURL)
}

// ValidateStripeWebhook checks the endpoint secret used to verify incoming
// Stripe signatures.
func (c Config) ValidateStripeWebhook() error {
	if c.StripeWebhookSecret == "" {
		return errors.New("STRIPE_WEBHOOK_SECRET is required for the API")
	}
	return nil
}

func validateReturnURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute HTTP or HTTPS URL", name)
	}
	return nil
}
