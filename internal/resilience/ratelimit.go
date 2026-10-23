package resilience

import (
	"math"
	"sync"
	"time"
)

// RateLimiter is a keyed token-bucket limiter (one bucket per client).
type RateLimiter struct {
	rate  float64 // tokens per second
	burst float64

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows a sustained rate of rps requests per key with bursts
// of up to burst requests. rps <= 0 disables limiting.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	return &RateLimiter{
		rate:    rps,
		burst:   float64(max(burst, 1)),
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// Allow consumes a token for key. When it returns false, retryAfter is the
// time until a token will be available.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if l == nil || l.rate <= 0 {
		return true, 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	b, found := l.buckets[key]
	if !found {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	need := (1 - b.tokens) / l.rate
	return false, time.Duration(need * float64(time.Second))
}

// sweep drops buckets that have refilled completely; they are
// indistinguishable from new buckets, so memory stays bounded by the number
// of recently active keys. Caller holds l.mu.
func (l *RateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
		}
	}
}
