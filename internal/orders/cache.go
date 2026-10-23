package orders

import (
	"container/list"
	"sync"
	"time"
)

// staleCache is a bounded LRU of last-known-good values (single orders and
// order lists). It is consulted only when the upstream fails, so customers
// still see real (clearly labelled, timestamped) data during an outage
// instead of an error.
type staleCache[V any] struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
}

type cacheEntry[V any] struct {
	id        string
	value     V
	fetchedAt time.Time
}

func newStaleCache[V any](ttl time.Duration, size int) *staleCache[V] {
	if ttl <= 0 || size <= 0 {
		return nil // disabled; all methods are nil-safe
	}
	return &staleCache[V]{ttl: ttl, max: size, now: time.Now, ll: list.New(), items: make(map[string]*list.Element)}
}

func (c *staleCache[V]) put(id string, v V, at time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		el.Value = cacheEntry[V]{id, v, at}
		c.ll.MoveToFront(el)
		return
	}
	c.items[id] = c.ll.PushFront(cacheEntry[V]{id, v, at})
	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(cacheEntry[V]).id)
	}
}

func (c *staleCache[V]) get(id string) (cacheEntry[V], bool) {
	if c == nil {
		return cacheEntry[V]{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[id]
	if !ok {
		return cacheEntry[V]{}, false
	}
	e := el.Value.(cacheEntry[V])
	if c.now().Sub(e.fetchedAt) > c.ttl {
		c.ll.Remove(el)
		delete(c.items, id)
		return cacheEntry[V]{}, false
	}
	return e, true
}

func (c *staleCache[V]) remove(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		c.ll.Remove(el)
		delete(c.items, id)
	}
}

func (c *staleCache[V]) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *staleCache[V]) purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	clear(c.items)
}

// each calls fn for every unexpired entry, most recently stored first, until fn returns false.
func (c *staleCache[V]) each(fn func(e cacheEntry[V]) bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	entries := make([]cacheEntry[V], 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		if e := el.Value.(cacheEntry[V]); c.now().Sub(e.fetchedAt) <= c.ttl {
			entries = append(entries, e)
		}
	}
	c.mu.Unlock()
	for _, e := range entries {
		if !fn(e) {
			return
		}
	}
}
