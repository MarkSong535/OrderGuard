package resilience

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errTransient = errors.New("transient")
var errPermanent = errors.New("permanent")

func isTransientErr(err error) bool { return errors.Is(err, errTransient) }

func fastRetrier(attempts int) Retrier {
	return Retrier{
		Policy:      RetryPolicy{MaxAttempts: attempts, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		ShouldRetry: isTransientErr,
	}
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	var calls int
	err := fastRetrier(3).Do(context.Background(), func(context.Context, int) error {
		calls++
		if calls < 3 {
			return errTransient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do() = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetryStopsAtMaxAttempts(t *testing.T) {
	var calls int
	err := fastRetrier(4).Do(context.Background(), func(context.Context, int) error {
		calls++
		return errTransient
	})
	if !errors.Is(err, errTransient) || calls != 4 {
		t.Fatalf("Do() = %v after %d calls, want errTransient after 4", err, calls)
	}
}

func TestRetryDoesNotRetryPermanentErrors(t *testing.T) {
	var calls int
	err := fastRetrier(5).Do(context.Background(), func(context.Context, int) error {
		calls++
		return errPermanent
	})
	if !errors.Is(err, errPermanent) || calls != 1 {
		t.Fatalf("Do() = %v after %d calls, want errPermanent after 1", err, calls)
	}
}

func TestRetryStopsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := Retrier{Policy: RetryPolicy{MaxAttempts: 10, BaseDelay: time.Hour}, ShouldRetry: isTransientErr}
	var calls int
	done := make(chan error, 1)
	go func() {
		done <- r.Do(ctx, func(context.Context, int) error { calls++; return errTransient })
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errTransient) || calls != 1 {
			t.Fatalf("Do() = %v after %d calls", err, calls)
		}
	case <-time.After(time.Second):
		t.Fatal("Do did not return after cancellation")
	}
}

func TestRetrySkipsDelayThatWouldOverrunDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := Retrier{Policy: RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second}, ShouldRetry: isTransientErr}
	start := time.Now()
	var calls int
	err := r.Do(ctx, func(context.Context, int) error { calls++; return errTransient })
	if !errors.Is(err, errTransient) || calls != 1 {
		t.Fatalf("Do() = %v after %d calls", err, calls)
	}
	if time.Since(start) > 30*time.Millisecond {
		t.Fatalf("Do slept %v instead of failing fast", time.Since(start))
	}
}

func TestRetryHonoursRetryAfter(t *testing.T) {
	r := fastRetrier(2)
	r.RetryAfter = func(error) (time.Duration, bool) { return 40 * time.Millisecond, true }
	var delays []time.Duration
	r.OnRetry = func(_ int, d time.Duration, _ error) { delays = append(delays, d) }
	start := time.Now()
	_ = r.Do(context.Background(), func(context.Context, int) error { return errTransient })
	if len(delays) != 1 || delays[0] != 40*time.Millisecond {
		t.Fatalf("delays = %v, want [40ms]", delays)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("retry did not wait for Retry-After")
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	p := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 350 * time.Millisecond}
	want := []time.Duration{100, 200, 350, 350}
	for i, w := range want {
		if got := p.Backoff(i + 1); got != w*time.Millisecond {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w*time.Millisecond)
		}
	}
	p.Jitter = true
	for range 100 {
		d := p.Backoff(2)
		if d < 100*time.Millisecond || d > 200*time.Millisecond {
			t.Fatalf("jittered Backoff(2) = %v, want within [100ms, 200ms]", d)
		}
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestBreaker(clock *fakeClock, transitions *[]string) *Breaker {
	return NewBreaker(BreakerConfig{
		Name:             "test",
		FailureThreshold: 3,
		OpenTimeout:      time.Second,
		IsFailure:        isTransientErr,
		Now:              clock.Now,
		OnStateChange: func(_ string, from, to State) {
			*transitions = append(*transitions, from.String()+"->"+to.String())
		},
	})
}

func TestBreakerLifecycle(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	var transitions []string
	b := newTestBreaker(clock, &transitions)

	fail := func() error { return errTransient }
	ok := func() error { return nil }

	for range 3 {
		_ = b.Execute(fail)
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %v after 3 failures, want open", b.State())
	}

	var called bool
	if err := b.Execute(func() error { called = true; return nil }); !errors.Is(err, ErrCircuitOpen) || called {
		t.Fatalf("open breaker: err = %v, called = %v", err, called)
	}
	if ra := b.RetryAfter(); ra != time.Second {
		t.Fatalf("RetryAfter = %v, want 1s", ra)
	}

	clock.Advance(time.Second)
	if b.State() != StateHalfOpen {
		t.Fatalf("state = %v after cooldown, want half_open", b.State())
	}
	_ = b.Execute(fail) // failed probe re-opens
	if b.State() != StateOpen {
		t.Fatalf("state = %v after failed probe, want open", b.State())
	}

	clock.Advance(time.Second)
	if err := b.Execute(ok); err != nil {
		t.Fatal(err)
	}
	if b.State() != StateClosed {
		t.Fatalf("state = %v after successful probe, want closed", b.State())
	}

	want := []string{"closed->open", "open->half_open", "half_open->open", "open->half_open", "half_open->closed"}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", transitions, want)
		}
	}
}

func TestBreakerIgnoresNonFailures(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	var tr []string
	b := newTestBreaker(clock, &tr)
	_ = b.Execute(func() error { return errTransient })
	_ = b.Execute(func() error { return errTransient })
	_ = b.Execute(func() error { return errPermanent }) // e.g. not found: dependency is healthy
	_ = b.Execute(func() error { return errTransient })
	_ = b.Execute(func() error { return errTransient })
	if b.State() != StateClosed {
		t.Fatalf("state = %v, want closed (non-failure must reset the streak)", b.State())
	}
}

func TestBreakerHalfOpenAdmitsLimitedProbes(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	var tr []string
	b := newTestBreaker(clock, &tr)
	for range 3 {
		_ = b.Execute(func() error { return errTransient })
	}
	clock.Advance(time.Second)

	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = b.Execute(func() error { close(started); <-release; return nil })
	}()
	<-started
	if err := b.Execute(func() error { return nil }); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second probe err = %v, want ErrCircuitOpen", err)
	}
	close(release)
}

func TestBreakerConcurrentUse(t *testing.T) {
	b := NewBreaker(BreakerConfig{FailureThreshold: 50, OpenTimeout: time.Millisecond, IsFailure: isTransientErr})
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			for j := range 200 {
				_ = b.Execute(func() error {
					if (i+j)%3 == 0 {
						return errTransient
					}
					return nil
				})
				_ = b.State()
			}
		})
	}
	wg.Wait()
}

func TestBulkheadBoundsConcurrency(t *testing.T) {
	bh := NewBulkhead(2, 20*time.Millisecond)
	r1, err := bh.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := bh.Acquire(context.Background())
	if _, err := bh.Acquire(context.Background()); !errors.Is(err, ErrBulkheadFull) {
		t.Fatalf("third Acquire err = %v, want ErrBulkheadFull", err)
	}
	r1()
	r1() // idempotent
	if bh.InUse() != 1 {
		t.Fatalf("InUse = %d, want 1", bh.InUse())
	}
	r3, err := bh.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2()
	r3()

	var peak, cur atomic.Int64
	var wg sync.WaitGroup
	bh = NewBulkhead(4, time.Second)
	for range 50 {
		wg.Go(func() {
			rel, err := bh.Acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			cur.Add(-1)
			rel()
		})
	}
	wg.Wait()
	if peak.Load() > 4 {
		t.Fatalf("peak concurrency %d exceeds limit 4", peak.Load())
	}
}

func TestGroupCoalescesConcurrentCalls(t *testing.T) {
	var g Group[string, int]
	var calls atomic.Int64
	gate := make(chan struct{})
	fn := func(context.Context) (int, error) {
		calls.Add(1)
		<-gate
		return 42, nil
	}

	const n = 50
	var wg sync.WaitGroup
	var shared atomic.Int64
	results := make(chan int, n)
	for range n {
		wg.Go(func() {
			v, err, s := g.Do(context.Background(), "k", fn)
			if err != nil {
				t.Error(err)
			}
			if s {
				shared.Add(1)
			}
			results <- v
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(results)

	if calls.Load() != 1 {
		t.Fatalf("fn called %d times, want 1", calls.Load())
	}
	if shared.Load() != n-1 {
		t.Fatalf("shared = %d, want %d", shared.Load(), n-1)
	}
	for v := range results {
		if v != 42 {
			t.Fatalf("result %d, want 42", v)
		}
	}
}

func TestGroupCallerCancellationDoesNotCancelSharedWork(t *testing.T) {
	var g Group[string, int]
	gate := make(chan struct{})
	var sawCancel atomic.Bool
	fn := func(ctx context.Context) (int, error) {
		<-gate
		if ctx.Err() != nil {
			sawCancel.Store(true)
		}
		return 7, nil
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err, _ := g.Do(leaderCtx, "k", fn)
		leaderDone <- err
	}()
	time.Sleep(10 * time.Millisecond)

	followerDone := make(chan int, 1)
	go func() {
		v, _, _ := g.Do(context.Background(), "k", fn)
		followerDone <- v
	}()
	time.Sleep(10 * time.Millisecond)

	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want context.Canceled", err)
	}
	close(gate)
	if v := <-followerDone; v != 7 {
		t.Fatalf("follower got %d, want 7", v)
	}
	if sawCancel.Load() {
		t.Fatal("shared work observed the leader's cancellation")
	}
}

func TestGroupRecoversPanics(t *testing.T) {
	var g Group[string, int]
	_, err, _ := g.Do(context.Background(), "k", func(context.Context) (int, error) { panic("boom") })
	if err == nil {
		t.Fatal("expected error from panicking fn")
	}
	v, err, _ := g.Do(context.Background(), "k", func(context.Context) (int, error) { return 1, nil })
	if err != nil || v != 1 {
		t.Fatalf("group unusable after panic: %v, %v", v, err)
	}
}

func TestRateLimiter(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	l := NewRateLimiter(2, 3)
	l.now = clock.Now

	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d within burst rejected", i)
		}
	}
	ok, retry := l.Allow("a")
	if ok {
		t.Fatal("request beyond burst allowed")
	}
	if retry != 500*time.Millisecond {
		t.Fatalf("retryAfter = %v, want 500ms", retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("keys must have independent buckets")
	}
	clock.Advance(500 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("token not refilled")
	}

	clock.Advance(2 * time.Minute)
	_, _ = l.Allow("c") // triggers sweep
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("buckets after sweep = %d, want 1", n)
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	l := NewRateLimiter(0, 0)
	for range 1000 {
		if ok, _ := l.Allow("x"); !ok {
			t.Fatal("disabled limiter rejected a request")
		}
	}
}
