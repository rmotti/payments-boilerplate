package ratelimit

import (
	"container/list"
	"fmt"
	"sync"
	"time"
)

// Limiter enforces one Policy across many keys, holding a bounded number of
// buckets.
//
// The bound is the point of the type. Keys are derived from what a client
// sends, so an unbounded map would turn a limiter meant to protect the process
// into the cheapest way to exhaust its memory. Two mechanisms keep it small:
//
//   - a hard capacity, enforced by evicting the least recently used bucket
//     when a new key arrives at a full limiter;
//   - an idle TTL, so a bucket nobody has touched for a while is reclaimed
//     even while the limiter sits below capacity.
//
// Eviction is safe in one direction only, and deliberately so. Losing a bucket
// hands its key a fresh burst, which is why capacity must exceed the number of
// callers a deployment actually has; it never denies a caller that should have
// been allowed. A full bucket is also indistinguishable from an absent one, so
// dropping an idle entry loses no enforcement at all.
//
// All methods are safe for concurrent use.
type Limiter struct {
	policy   Policy
	capacity int
	ttl      time.Duration
	clock    Clock

	mu sync.Mutex
	// order is the LRU list, most recently used at the front. Its elements
	// carry the key, so eviction can find the map entry to delete.
	order *list.List
	// buckets maps a key to its element in order. The element's value holds
	// the bucket itself, so one lookup reaches both.
	buckets map[string]*list.Element
}

// entry is what an LRU element carries.
type entry struct {
	key    string
	bucket bucket
}

// Options configure a Limiter beyond its policy.
type Options struct {
	// Capacity is the maximum number of buckets held at once. It must be
	// positive.
	Capacity int
	// TTL reclaims a bucket untouched for this long. Zero disables expiry and
	// leaves capacity as the only bound.
	TTL time.Duration
	// Clock reads time. Nil means time.Now.
	Clock Clock
}

// New builds a limiter enforcing policy over at most opts.Capacity keys.
func New(policy Policy, opts Options, name string) (*Limiter, error) {
	if err := policy.Validate(name); err != nil {
		return nil, err
	}
	if opts.Capacity < 1 {
		return nil, fmt.Errorf("%s capacity must be at least 1", name)
	}
	if opts.TTL < 0 {
		return nil, fmt.Errorf("%s TTL must not be negative", name)
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{
		policy:   policy,
		capacity: opts.Capacity,
		ttl:      opts.TTL,
		clock:    clock,
		order:    list.New(),
		buckets:  make(map[string]*list.Element),
	}, nil
}

// Allow spends one token from key's bucket, creating it full if absent.
//
// A rejected request costs nothing, so a client hammering a spent bucket does
// not push its own Retry-After further into the future.
func (l *Limiter) Allow(key string) Decision {
	now := l.clock()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.expire(now)

	element, found := l.buckets[key]
	if !found {
		l.evictIfFull()
		element = l.order.PushFront(&entry{
			key: key,
			// A new key starts full: it has not spent anything yet, and
			// starting it empty would reject a first-time caller.
			bucket: bucket{tokens: float64(l.policy.Burst), updatedAt: now},
		})
		l.buckets[key] = element
	} else {
		l.order.MoveToFront(element)
	}

	return element.Value.(*entry).bucket.take(l.policy, now)
}

// expire drops buckets untouched for longer than the TTL. The list is ordered
// by use, so the walk stops at the first entry still young enough: everything
// ahead of it is younger still.
func (l *Limiter) expire(now time.Time) {
	if l.ttl <= 0 {
		return
	}
	for {
		oldest := l.order.Back()
		if oldest == nil {
			return
		}
		held := oldest.Value.(*entry)
		if now.Sub(held.bucket.updatedAt) < l.ttl {
			return
		}
		l.order.Remove(oldest)
		delete(l.buckets, held.key)
	}
}

// evictIfFull makes room for one new key by dropping the least recently used
// bucket. Its key gets a fresh burst, which is why capacity is sized above the
// caller population a deployment expects rather than at it.
func (l *Limiter) evictIfFull() {
	for l.order.Len() >= l.capacity {
		oldest := l.order.Back()
		if oldest == nil {
			return
		}
		l.order.Remove(oldest)
		delete(l.buckets, oldest.Value.(*entry).key)
	}
}

// Len reports how many buckets are currently held. It exists for the tests
// that prove the bound; nothing in the request path reads it.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}
