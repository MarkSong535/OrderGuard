package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBulkheadFull is returned when no slot became free within the wait budget.
var ErrBulkheadFull = errors.New("bulkhead capacity exhausted")

// Bulkhead bounds the number of concurrent operations against a dependency so
// that a slow upstream cannot consume every goroutine, socket and file
// descriptor in the process.
type Bulkhead struct {
	slots   chan struct{}
	maxWait time.Duration
}

// NewBulkhead allows up to limit concurrent holders; callers wait at most
// maxWait for a slot.
func NewBulkhead(limit int, maxWait time.Duration) *Bulkhead {
	return &Bulkhead{slots: make(chan struct{}, max(limit, 1)), maxWait: maxWait}
}

// Acquire reserves a slot. The returned release func is idempotent.
func (b *Bulkhead) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case b.slots <- struct{}{}:
		return b.releaser(), nil
	default:
	}
	if b.maxWait <= 0 {
		return nil, ErrBulkheadFull
	}
	t := time.NewTimer(b.maxWait)
	defer t.Stop()
	select {
	case b.slots <- struct{}{}:
		return b.releaser(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, ErrBulkheadFull
	}
}

func (b *Bulkhead) releaser() func() {
	var once sync.Once
	return func() { once.Do(func() { <-b.slots }) }
}

// InUse reports the number of held slots.
func (b *Bulkhead) InUse() int { return len(b.slots) }

// Capacity reports the slot limit.
func (b *Bulkhead) Capacity() int { return cap(b.slots) }
