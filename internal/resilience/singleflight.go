package resilience

import (
	"context"
	"fmt"
	"sync"
)

// Group coalesces concurrent calls that share a key into a single execution,
// so a burst of lookups for the same order costs one upstream request.
//
// Unlike golang.org/x/sync/singleflight, the shared work runs on a context
// detached from any single caller's cancellation: one impatient client
// disconnecting does not fail the call for everyone else. fn must therefore
// apply its own deadline. Each caller still stops waiting when its own
// context is done.
type Group[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*call[V]
}

type call[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// Do executes fn once per key among concurrent callers. shared reports whether
// this caller joined a call started by another caller.
func (g *Group[K, V]) Do(ctx context.Context, key K, fn func(context.Context) (V, error)) (v V, err error, shared bool) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[K]*call[V])
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		v, err = wait(ctx, c)
		return v, err, true
	}
	c := &call[V]{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.err = fmt.Errorf("panic in coalesced call: %v", r)
			}
			g.mu.Lock()
			delete(g.calls, key)
			g.mu.Unlock()
			close(c.done)
		}()
		c.val, c.err = fn(context.WithoutCancel(ctx))
	}()

	v, err = wait(ctx, c)
	return v, err, false
}

func wait[V any](ctx context.Context, c *call[V]) (V, error) {
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}
