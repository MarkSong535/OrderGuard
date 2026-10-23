// Package resilience contains the fault-tolerance primitives used around
// upstream calls: bounded retries with backoff, a circuit breaker, a
// bulkhead, request coalescing and a token-bucket rate limiter.
package resilience

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

// RetryPolicy configures bounded exponential backoff.
type RetryPolicy struct {
	MaxAttempts int           // total attempts including the first; <1 is treated as 1
	BaseDelay   time.Duration // delay before the first retry
	MaxDelay    time.Duration // cap on any single delay
	Multiplier  float64       // growth factor; <=1 defaults to 2
	Jitter      bool          // randomise delays to avoid synchronized retry storms
}

// Backoff returns the delay to wait after the given failed attempt (1-based).
// With jitter enabled it uses "equal jitter": half the computed delay plus a
// random amount up to the other half, so delays never collapse to zero.
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	mult := p.Multiplier
	if mult <= 1 {
		mult = 2
	}
	d := float64(p.BaseDelay) * math.Pow(mult, float64(attempt-1))
	if p.MaxDelay > 0 && d > float64(p.MaxDelay) {
		d = float64(p.MaxDelay)
	}
	if p.Jitter && d > 0 {
		d = d/2 + rand.Float64()*d/2
	}
	return time.Duration(d)
}

// Retrier executes an operation under a RetryPolicy.
type Retrier struct {
	Policy RetryPolicy
	// ShouldRetry decides whether an error is transient. Nil means never retry.
	ShouldRetry func(error) bool
	// RetryAfter extracts a server-provided minimum delay (e.g. HTTP Retry-After).
	RetryAfter func(error) (time.Duration, bool)
	// OnRetry is invoked before sleeping ahead of the next attempt.
	OnRetry func(attempt int, delay time.Duration, err error)
}

// Do calls fn until it succeeds, returns a non-retryable error, the attempt
// budget is exhausted, or ctx is done. A retry is skipped entirely when the
// required delay would overrun the context deadline, so callers fail fast
// instead of sleeping into a guaranteed timeout. Do returns the last error
// produced by fn (or ctx.Err() if fn was never called).
func (r Retrier) Do(ctx context.Context, fn func(ctx context.Context, attempt int) error) error {
	maxAttempts := max(r.Policy.MaxAttempts, 1)
	var err error
	for attempt := 1; ; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			if err != nil {
				return err
			}
			return cerr
		}
		err = fn(ctx, attempt)
		if err == nil {
			return nil
		}
		if attempt >= maxAttempts || r.ShouldRetry == nil || !r.ShouldRetry(err) || ctx.Err() != nil {
			return err
		}

		delay := r.Policy.Backoff(attempt)
		if r.RetryAfter != nil {
			if ra, ok := r.RetryAfter(err); ok && ra > delay {
				delay = ra
			}
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
			return err
		}
		if r.OnRetry != nil {
			r.OnRetry(attempt, delay, err)
		}

		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}
