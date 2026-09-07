package outbox

import (
	"math"
	"math/rand/v2"
	"time"
)

// Backoff computes how long a failed message waits before its next attempt.
//
// The delay grows exponentially and is capped, so a broker that is down for an
// hour is retried patiently instead of being hammered. The jitter spreads
// retries out: without it, every message that failed during one outage would
// come due at the same instant and stampede the broker the moment it returns.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Backoff defaults.
const (
	DefaultBackoffBase = 2 * time.Second
	DefaultBackoffMax  = 5 * time.Minute
)

func (b Backoff) withDefaults() Backoff {
	if b.Base <= 0 {
		b.Base = DefaultBackoffBase
	}
	if b.Max <= 0 {
		b.Max = DefaultBackoffMax
	}
	return b
}

// Delay returns the wait before attempt number attempts+1.
func (b Backoff) Delay(attempts int) time.Duration {
	b = b.withDefaults()
	if attempts < 0 {
		attempts = 0
	}
	// Cap the exponent before shifting so a long outage cannot overflow.
	const maxExponent = 20
	exponent := math.Min(float64(attempts), maxExponent)
	delay := time.Duration(float64(b.Base) * math.Pow(2, exponent))
	if delay <= 0 || delay > b.Max {
		delay = b.Max
	}
	// Full jitter: anywhere in (0, delay]. Keeps retries from synchronizing.
	jittered := time.Duration(rand.Int64N(int64(delay))) + time.Millisecond
	if jittered > delay {
		jittered = delay
	}
	return jittered
}
