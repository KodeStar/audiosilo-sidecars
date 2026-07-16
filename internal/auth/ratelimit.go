package auth

import (
	"sync"
	"time"
)

// RateLimiter is a small per-key token-bucket limiter, used to throttle login
// attempts per client IP. Refill is lazy (computed on access), so there is no
// background goroutine. It is safe for concurrent use.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	burst   float64
	refill  float64 // tokens added per second
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter returns a limiter allowing bursts of burst attempts, refilling
// at refillPerSec tokens per second.
func NewRateLimiter(burst int, refillPerSec float64) *RateLimiter {
	return &RateLimiter{
		buckets: map[string]*bucket{},
		burst:   float64(burst),
		refill:  refillPerSec,
		now:     time.Now,
	}
}

// Allow reports whether a request keyed by key may proceed, consuming one token
// when it returns true.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[key] = b
	}
	// Lazy refill.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * r.refill
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	// Opportunistically prune fully-refilled buckets for other keys to bound
	// memory (this key is not full since we just failed it).
	r.prune(now)
	return false
}

// prune drops buckets that have refilled to full and are therefore
// indistinguishable from a fresh one.
func (r *RateLimiter) prune(now time.Time) {
	for k, b := range r.buckets {
		elapsed := now.Sub(b.last).Seconds()
		if b.tokens+elapsed*r.refill >= r.burst {
			delete(r.buckets, k)
		}
	}
}
