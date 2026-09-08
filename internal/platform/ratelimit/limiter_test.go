package ratelimit

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is the fake time source. Every test here drives it by hand, so refill
// and expiry are proven exactly and the package's tests contain no sleeps.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestLimiter(t *testing.T, policy Policy, opts Options) *Limiter {
	t.Helper()
	limiter, err := New(policy, opts, "TEST")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return limiter
}

func TestBurstIsSpentThenRefilledOneTokenAtATime(t *testing.T) {
	t.Parallel()

	fake := newClock()
	policy := Policy{Burst: 4, Interval: 4 * time.Second}
	limiter := newTestLimiter(t, policy, Options{Capacity: 8, Clock: fake.Now})

	for attempt := 1; attempt <= policy.Burst; attempt++ {
		if decision := limiter.Allow("a"); !decision.Allowed {
			t.Fatalf("request %d inside the burst was refused", attempt)
		}
	}
	if decision := limiter.Allow("a"); decision.Allowed {
		t.Fatal("a request past the burst was allowed")
	}

	// One token is Interval/Burst, one second here. Just short of it, nothing.
	fake.Advance(999 * time.Millisecond)
	if decision := limiter.Allow("a"); decision.Allowed {
		t.Fatal("a token accrued before its interval had elapsed")
	}
	fake.Advance(time.Millisecond)
	if decision := limiter.Allow("a"); !decision.Allowed {
		t.Fatal("a token did not accrue after its full interval")
	}
	// And that single token is all that had accrued.
	if decision := limiter.Allow("a"); decision.Allowed {
		t.Fatal("more than one token accrued in one token interval")
	}
}

// A refused request costs nothing. Without this, a client retrying in a tight
// loop would keep pushing its own Retry-After further out.
func TestRejectionConsumesNoCredit(t *testing.T) {
	t.Parallel()

	fake := newClock()
	policy := Policy{Burst: 1, Interval: time.Second}
	limiter := newTestLimiter(t, policy, Options{Capacity: 4, Clock: fake.Now})

	if decision := limiter.Allow("a"); !decision.Allowed {
		t.Fatal("the first request was refused")
	}
	for attempt := 0; attempt < 100; attempt++ {
		limiter.Allow("a")
	}
	// One token's worth of time has to be enough, no matter how many refusals
	// happened in between.
	fake.Advance(time.Second)
	if decision := limiter.Allow("a"); !decision.Allowed {
		t.Fatal("refusals delayed the refill")
	}
}

// An idle bucket must not bank unlimited credit, or a caller could stay silent
// for a day and then spend the whole day's allowance at once.
func TestIdleBucketAccumulatesAtMostOneBurst(t *testing.T) {
	t.Parallel()

	fake := newClock()
	policy := Policy{Burst: 3, Interval: 3 * time.Second}
	limiter := newTestLimiter(t, policy, Options{Capacity: 4, Clock: fake.Now})

	limiter.Allow("a")
	fake.Advance(time.Hour)
	for attempt := 1; attempt <= policy.Burst; attempt++ {
		if decision := limiter.Allow("a"); !decision.Allowed {
			t.Fatalf("request %d after a long idle was refused", attempt)
		}
	}
	if decision := limiter.Allow("a"); decision.Allowed {
		t.Fatal("an idle bucket banked more than one burst")
	}
}

func TestRetryAfterIsAWholeSecondAndBoundedByTheInterval(t *testing.T) {
	t.Parallel()

	fake := newClock()
	policy := Policy{Burst: 10, Interval: 100 * time.Second}
	limiter := newTestLimiter(t, policy, Options{Capacity: 4, Clock: fake.Now})

	for attempt := 0; attempt < policy.Burst; attempt++ {
		limiter.Allow("a")
	}
	decision := limiter.Allow("a")
	if decision.Allowed {
		t.Fatal("the request past the burst was allowed")
	}
	// A token here is worth ten seconds, and none of it has accrued.
	if decision.RetryAfter != 10*time.Second {
		t.Fatalf("RetryAfter = %s, want 10s", decision.RetryAfter)
	}
	if decision.RetryAfter%time.Second != 0 {
		t.Fatalf("RetryAfter = %s, want a whole number of seconds", decision.RetryAfter)
	}

	// Half a token in, the remaining wait shrinks accordingly.
	fake.Advance(5 * time.Second)
	if decision := limiter.Allow("a"); decision.RetryAfter != 5*time.Second {
		t.Fatalf("RetryAfter after half a token = %s, want 5s", decision.RetryAfter)
	}

	// A sub-second wait still rounds up, so a client never retries instantly.
	fake.Advance(4500 * time.Millisecond)
	if decision := limiter.Allow("a"); decision.RetryAfter != time.Second {
		t.Fatalf("RetryAfter for a sub-second wait = %s, want 1s", decision.RetryAfter)
	}
}

func TestAllowedDecisionCarriesNoRetryAfter(t *testing.T) {
	t.Parallel()

	limiter := newTestLimiter(t, Policy{Burst: 2, Interval: time.Second}, Options{Capacity: 2})
	if decision := limiter.Allow("a"); !decision.Allowed || decision.RetryAfter != 0 {
		t.Fatalf("Allow() = %+v, want allowed with no RetryAfter", decision)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	t.Parallel()

	limiter := newTestLimiter(t, Policy{Burst: 1, Interval: time.Minute}, Options{Capacity: 8})
	if decision := limiter.Allow("a"); !decision.Allowed {
		t.Fatal("the first key was refused")
	}
	if decision := limiter.Allow("a"); decision.Allowed {
		t.Fatal("the first key was allowed past its burst")
	}
	if decision := limiter.Allow("b"); !decision.Allowed {
		t.Fatal("a second key was refused because of the first one's spending")
	}
}

// The memory bound is the property that makes this limiter safe to key on
// something a client controls. It must hold no matter how many keys arrive.
func TestCapacityBoundsTheNumberOfBuckets(t *testing.T) {
	t.Parallel()

	const capacity = 32
	limiter := newTestLimiter(t, Policy{Burst: 1, Interval: time.Minute}, Options{Capacity: capacity})

	for attempt := 0; attempt < 10_000; attempt++ {
		limiter.Allow("key-" + strconv.Itoa(attempt))
	}
	if held := limiter.Len(); held > capacity {
		t.Fatalf("limiter holds %d buckets, want at most %d", held, capacity)
	}
}

// Eviction must drop the least recently used key, so a caller that keeps
// spending keeps its bucket while one-shot keys pass through.
func TestEvictionDropsTheLeastRecentlyUsedKey(t *testing.T) {
	t.Parallel()

	fake := newClock()
	limiter := newTestLimiter(t, Policy{Burst: 1, Interval: time.Hour},
		Options{Capacity: 3, Clock: fake.Now})

	// "hot" spends its only token and is kept warm by being used again.
	limiter.Allow("hot")
	limiter.Allow("cold")
	limiter.Allow("hot")

	// A third and fourth key force eviction. "cold" is the least recently
	// used, so it goes first and "hot" must still be spent.
	limiter.Allow("new-1")
	limiter.Allow("new-2")

	if decision := limiter.Allow("hot"); decision.Allowed {
		t.Fatal("the recently used bucket was evicted and handed a fresh burst")
	}
	if decision := limiter.Allow("cold"); !decision.Allowed {
		t.Fatal("the least recently used bucket was not the one evicted")
	}
}

// The TTL reclaims buckets while the limiter is below capacity, so a quiet
// process shrinks instead of holding its high-water mark until restart.
func TestIdleBucketsExpire(t *testing.T) {
	t.Parallel()

	fake := newClock()
	const ttl = 10 * time.Minute
	limiter := newTestLimiter(t, Policy{Burst: 1, Interval: time.Minute},
		Options{Capacity: 1000, TTL: ttl, Clock: fake.Now})

	for attempt := 0; attempt < 100; attempt++ {
		limiter.Allow("key-" + strconv.Itoa(attempt))
	}
	if held := limiter.Len(); held != 100 {
		t.Fatalf("limiter holds %d buckets, want 100", held)
	}

	// Just short of the TTL nothing is reclaimed.
	fake.Advance(ttl - time.Second)
	limiter.Allow("probe")
	if held := limiter.Len(); held != 101 {
		t.Fatalf("limiter holds %d buckets before the TTL elapsed, want 101", held)
	}

	// Past it, every idle bucket goes, leaving only the one just touched.
	fake.Advance(2 * time.Second)
	limiter.Allow("probe")
	if held := limiter.Len(); held != 1 {
		t.Fatalf("limiter holds %d buckets after the TTL elapsed, want 1", held)
	}
}

// Expiry must not be visible as enforcement: a bucket reclaimed while idle was
// necessarily full, so its key sees the same answer either way.
func TestExpiryDoesNotChangeTheAnswerForAFullBucket(t *testing.T) {
	t.Parallel()

	fake := newClock()
	policy := Policy{Burst: 2, Interval: time.Minute}
	limiter := newTestLimiter(t, policy, Options{Capacity: 8, TTL: time.Hour, Clock: fake.Now})

	limiter.Allow("a")
	// Long enough to both refill the bucket and expire it.
	fake.Advance(2 * time.Hour)
	for attempt := 1; attempt <= policy.Burst; attempt++ {
		if decision := limiter.Allow("a"); !decision.Allowed {
			t.Fatalf("request %d after expiry was refused", attempt)
		}
	}
	if decision := limiter.Allow("a"); decision.Allowed {
		t.Fatal("an expired bucket gave more than one burst")
	}
}

// Under -race this is the guard against a lost update in the token accounting.
// With the clock frozen, exactly the burst may pass: more means a lost update,
// fewer means a spurious one.
func TestConcurrentAllowIsExactAndRaceFree(t *testing.T) {
	t.Parallel()

	fake := newClock()
	const burst = 100
	limiter := newTestLimiter(t, Policy{Burst: burst, Interval: time.Hour},
		Options{Capacity: 64, TTL: time.Hour, Clock: fake.Now})

	var allowed atomic.Int64
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for attempt := 0; attempt < 500; attempt++ {
				if limiter.Allow("shared").Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	group.Wait()

	if got := allowed.Load(); got != burst {
		t.Fatalf("allowed %d concurrent requests, want exactly %d", got, burst)
	}
}

// Concurrency over many keys exercises the LRU list and the map together,
// which is where a race would most likely hide.
func TestConcurrentDistinctKeysStayWithinCapacity(t *testing.T) {
	t.Parallel()

	const capacity = 64
	limiter := newTestLimiter(t, Policy{Burst: 2, Interval: time.Minute},
		Options{Capacity: capacity, TTL: time.Minute})

	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for attempt := 0; attempt < 500; attempt++ {
				limiter.Allow("worker-" + strconv.Itoa(worker) + "-key-" + strconv.Itoa(attempt))
			}
		}(worker)
	}
	group.Wait()

	if held := limiter.Len(); held > capacity {
		t.Fatalf("limiter holds %d buckets, want at most %d", held, capacity)
	}
}

func TestInvalidConfigurationIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy Policy
		opts   Options
	}{
		{"zero burst", Policy{Burst: 0, Interval: time.Second}, Options{Capacity: 1}},
		{"negative burst", Policy{Burst: -1, Interval: time.Second}, Options{Capacity: 1}},
		{"zero interval", Policy{Burst: 1, Interval: 0}, Options{Capacity: 1}},
		{"negative interval", Policy{Burst: 1, Interval: -time.Second}, Options{Capacity: 1}},
		{"interval too short for the burst", Policy{Burst: 1000, Interval: 1}, Options{Capacity: 1}},
		{"zero capacity", Policy{Burst: 1, Interval: time.Second}, Options{Capacity: 0}},
		{"negative capacity", Policy{Burst: 1, Interval: time.Second}, Options{Capacity: -1}},
		{"negative TTL", Policy{Burst: 1, Interval: time.Second}, Options{Capacity: 1, TTL: -time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(test.policy, test.opts, "TEST"); err == nil {
				t.Fatal("New() accepted an invalid configuration")
			}
		})
	}
}

func TestZeroTTLDisablesExpiry(t *testing.T) {
	t.Parallel()

	fake := newClock()
	limiter := newTestLimiter(t, Policy{Burst: 1, Interval: time.Minute},
		Options{Capacity: 8, TTL: 0, Clock: fake.Now})

	limiter.Allow("a")
	fake.Advance(365 * 24 * time.Hour)
	limiter.Allow("b")
	if held := limiter.Len(); held != 2 {
		t.Fatalf("limiter holds %d buckets with expiry disabled, want 2", held)
	}
}
