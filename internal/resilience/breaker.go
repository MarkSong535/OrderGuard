package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State is a circuit breaker state.
type State int

const (
	StateClosed State = iota
	StateHalfOpen
	StateOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half_open"
	case StateOpen:
		return "open"
	}
	return "unknown"
}

// ErrCircuitOpen is returned without calling the operation while the breaker
// is open (or half-open with its probe budget in use).
var ErrCircuitOpen = errors.New("circuit breaker is open")

// BreakerConfig configures a Breaker.
type BreakerConfig struct {
	Name string
	// FailureThreshold is the number of consecutive failures that opens the circuit.
	FailureThreshold int
	// OpenTimeout is how long the circuit stays open before allowing probes.
	OpenTimeout time.Duration
	// HalfOpenMaxCalls bounds concurrent probe calls in the half-open state.
	HalfOpenMaxCalls int
	// SuccessThreshold is the number of successful probes needed to close.
	SuccessThreshold int
	// IsFailure classifies errors. Errors that are not failures (for example
	// "not found") count as evidence that the dependency is healthy.
	// Nil treats every error except context.Canceled as a failure.
	IsFailure func(error) bool
	// OnStateChange is called (outside the breaker lock) on every transition.
	OnStateChange func(name string, from, to State)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Breaker is a consecutive-failure circuit breaker, safe for concurrent use.
type Breaker struct {
	cfg BreakerConfig

	mu                sync.Mutex
	state             State
	generation        uint64 // bumps on each transition; stale results are ignored
	failures          int
	openedAt          time.Time
	halfOpenInFlight  int
	halfOpenSuccesses int
}

// NewBreaker returns a closed breaker, filling in defaults.
func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = 10 * time.Second
	}
	if cfg.HalfOpenMaxCalls <= 0 {
		cfg.HalfOpenMaxCalls = 1
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = 1
	}
	if cfg.IsFailure == nil {
		cfg.IsFailure = func(err error) bool { return !errors.Is(err, context.Canceled) }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Breaker{cfg: cfg}
}

type transition struct{ from, to State }

// Execute runs fn if the breaker admits the call and records its outcome.
func (b *Breaker) Execute(fn func() error) (err error) {
	gen, err := b.before()
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			b.after(gen, errors.New("panic"))
			panic(r)
		}
		b.after(gen, err)
	}()
	return fn()
}

// State returns the current state, applying any due open→half-open transition.
func (b *Breaker) State() State {
	b.mu.Lock()
	t := b.refresh(b.cfg.Now())
	s := b.state
	b.mu.Unlock()
	b.notify(t)
	return s
}

// RetryAfter returns how long until an open breaker admits probes (0 otherwise).
func (b *Breaker) RetryAfter() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != StateOpen {
		return 0
	}
	return max(b.openedAt.Add(b.cfg.OpenTimeout).Sub(b.cfg.Now()), 0)
}

func (b *Breaker) before() (uint64, error) {
	b.mu.Lock()
	t := b.refresh(b.cfg.Now())
	var err error
	switch b.state {
	case StateOpen:
		err = ErrCircuitOpen
	case StateHalfOpen:
		if b.halfOpenInFlight >= b.cfg.HalfOpenMaxCalls {
			err = ErrCircuitOpen
		} else {
			b.halfOpenInFlight++
		}
	}
	gen := b.generation
	b.mu.Unlock()
	b.notify(t)
	return gen, err
}

func (b *Breaker) after(gen uint64, err error) {
	failure := err != nil && b.cfg.IsFailure(err)

	b.mu.Lock()
	var t []transition
	if gen == b.generation {
		switch b.state {
		case StateClosed:
			if failure {
				b.failures++
				if b.failures >= b.cfg.FailureThreshold {
					t = append(t, b.setState(StateOpen))
				}
			} else {
				b.failures = 0
			}
		case StateHalfOpen:
			b.halfOpenInFlight--
			if failure {
				t = append(t, b.setState(StateOpen))
			} else {
				b.halfOpenSuccesses++
				if b.halfOpenSuccesses >= b.cfg.SuccessThreshold {
					t = append(t, b.setState(StateClosed))
				}
			}
		}
	}
	b.mu.Unlock()
	b.notify(t)
}

// refresh moves an expired open breaker to half-open. Caller holds b.mu.
func (b *Breaker) refresh(now time.Time) []transition {
	if b.state == StateOpen && !now.Before(b.openedAt.Add(b.cfg.OpenTimeout)) {
		return []transition{b.setState(StateHalfOpen)}
	}
	return nil
}

// setState performs a transition. Caller holds b.mu.
func (b *Breaker) setState(to State) transition {
	t := transition{b.state, to}
	b.state = to
	b.generation++
	b.failures = 0
	b.halfOpenInFlight = 0
	b.halfOpenSuccesses = 0
	if to == StateOpen {
		b.openedAt = b.cfg.Now()
	}
	return t
}

func (b *Breaker) notify(ts []transition) {
	if b.cfg.OnStateChange == nil {
		return
	}
	for _, t := range ts {
		b.cfg.OnStateChange(b.cfg.Name, t.from, t.to)
	}
}
