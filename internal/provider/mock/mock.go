// Package mock implements an in-memory Provider with optional fault
// injection, for local development and tests.
package mock

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"orderguard/internal/provider"
)

const op = "mock.GetOrder"

// Provider serves orders from memory.
type Provider struct {
	mu          sync.RWMutex
	orders      map[string]*provider.Order
	latency     time.Duration
	failureRate float64
	calls       atomic.Int64
}

// Option configures a Provider.
type Option func(*Provider)

// WithLatency delays every call by d (respecting cancellation).
func WithLatency(d time.Duration) Option { return func(p *Provider) { p.latency = d } }

// WithFailureRate makes a fraction r (0..1) of calls fail as unavailable.
func WithFailureRate(r float64) Option { return func(p *Provider) { p.failureRate = r } }

// New returns a Provider holding orders.
func New(orders []provider.Order, opts ...Option) *Provider {
	p := &Provider{orders: make(map[string]*provider.Order, len(orders))}
	for i := range orders {
		o := orders[i]
		p.orders[o.ID] = &o
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "mock" }

// Calls returns the number of GetOrder invocations.
func (p *Provider) Calls() int64 { return p.calls.Load() }

// GetOrder implements provider.Provider.
func (p *Provider) GetOrder(ctx context.Context, id string) (*provider.Order, error) {
	if err := p.simulate(ctx); err != nil {
		return nil, err
	}
	p.mu.RLock()
	o, ok := p.orders[id]
	p.mu.RUnlock()
	if !ok {
		return nil, provider.NotFound(op)
	}
	return o, nil
}

// simulate applies the configured latency and failure rate.
func (p *Provider) simulate(ctx context.Context) error {
	p.calls.Add(1)
	if p.latency > 0 {
		t := time.NewTimer(p.latency)
		select {
		case <-ctx.Done():
			t.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return &provider.Error{Kind: provider.KindTimeout, Op: op, Err: ctx.Err()}
			}
			return ctx.Err()
		case <-t.C:
		}
	}
	if p.failureRate > 0 && rand.Float64() < p.failureRate {
		return &provider.Error{Kind: provider.KindUnavailable, Op: op, Err: errors.New("injected failure")}
	}
	return nil
}

// ListOrders implements provider.Provider.
func (p *Provider) ListOrders(ctx context.Context, f provider.ListFilter) ([]*provider.Order, error) {
	if err := p.simulate(ctx); err != nil {
		return nil, err
	}
	p.mu.RLock()
	out := make([]*provider.Order, 0, len(p.orders))
	for _, o := range p.orders {
		if f.CustomerID == "" || o.CustomerID == f.CustomerID {
			out = append(out, o)
		}
	}
	p.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if n := f.Clamped(); len(out) > n {
		out = out[:n]
	}
	return out, nil
}
