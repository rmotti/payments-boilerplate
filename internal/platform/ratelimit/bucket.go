// Package ratelimit implements the token bucket the HTTP boundary uses to
// bound how fast one caller may spend an operation.
//
// It knows nothing about HTTP, addresses or credentials. The transport layer
// decides what a key means and which policy applies to which route; this
// package only answers whether a key has a token left and, when it does not,
// how long the caller must wait for one.
//
// Two properties shape the implementation. Time is injected, so a test proves
// refill without sleeping. And memory is bounded by construction: a Limiter
// holds at most Capacity buckets, evicting the least recently used, because
// every key it is asked about ultimately comes from something the client
// sends.
package ratelimit

import (
	"fmt"
	"math"
	"time"
)

// Clock reads the current time. Production passes time.Now; a test passes a
// counter it advances by hand, which is what lets refill be proven exactly
// rather than approximately.
type Clock func() time.Time

// Policy is one limit: a sustained rate plus the burst a caller may spend at
// once. Burst is also the bucket's ceiling, so an idle caller accumulates at
// most one burst of credit rather than an unbounded one.
type Policy struct {
	// Burst is the number of requests allowed back to back from a full
	// bucket. It must be at least one, or nothing would ever pass.
	Burst int
	// Interval is how long the bucket takes to refill one whole burst. The
	// sustained rate is therefore Burst per Interval, and one token is worth
	// Interval/Burst.
	Interval time.Duration
}

// Validate reports whether the policy can be enforced. A zero burst would
// reject every request, and a non-positive interval would make the refill
// rate infinite, so both are configuration errors rather than edge cases to
// absorb silently.
func (p Policy) Validate(name string) error {
	if p.Burst < 1 {
		return fmt.Errorf("%s burst must be at least 1", name)
	}
	if p.Interval <= 0 {
		return fmt.Errorf("%s interval must be positive", name)
	}
	// One token must be representable as a duration a caller can wait for.
	// A burst so large that Interval/Burst truncates to zero would make
	// Retry-After meaningless.
	if p.Interval/time.Duration(p.Burst) <= 0 {
		return fmt.Errorf("%s interval is too short to refill %d tokens", name, p.Burst)
	}
	return nil
}

// tokenInterval is how long one token takes to accrue.
func (p Policy) tokenInterval() time.Duration {
	return p.Interval / time.Duration(p.Burst)
}

// Decision is the outcome of one Allow call.
type Decision struct {
	// Allowed reports whether the request may proceed. A rejected request
	// consumes nothing, so a caller cannot push its own wait further out by
	// retrying in a tight loop.
	Allowed bool
	// RetryAfter is how long until the next token accrues. It is zero when
	// the request was allowed, and always at least one second when it was
	// not, because Retry-After is expressed in whole seconds and rounding a
	// sub-second wait down to zero would invite an immediate retry.
	RetryAfter time.Duration
}

// bucket is the mutable state of one key. It is stored by value inside the
// limiter's map and only ever touched under the limiter's lock.
type bucket struct {
	// tokens is the credit available at updatedAt, as a fraction so that
	// partial refills are not lost to truncation across calls.
	tokens float64
	// updatedAt is when tokens was last computed.
	updatedAt time.Time
}

// take refills the bucket up to now and spends one token when one is there.
func (b *bucket) take(policy Policy, now time.Time) Decision {
	burst := float64(policy.Burst)
	if elapsed := now.Sub(b.updatedAt); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed.Seconds()/policy.tokenInterval().Seconds())
	}
	b.updatedAt = now

	if b.tokens >= 1 {
		b.tokens--
		return Decision{Allowed: true}
	}
	// The wait is what the missing fraction of one token costs at the
	// configured refill rate, rounded up to the second Retry-After speaks in.
	missing := 1 - b.tokens
	wait := time.Duration(missing * float64(policy.tokenInterval()))
	return Decision{Allowed: false, RetryAfter: roundUpToSecond(wait)}
}

func roundUpToSecond(wait time.Duration) time.Duration {
	if wait <= time.Second {
		return time.Second
	}
	rounded := wait.Truncate(time.Second)
	if rounded < wait {
		rounded += time.Second
	}
	return rounded
}

// Key renders a value as a limiter key. It accepts anything with a String
// method, which is what the transport layer's address prefixes have, so the
// caller does not have to remember which spelling the limiter expects.
func Key(value fmt.Stringer) string { return value.String() }
