// Package orders implements the order lookup use case: input validation,
// fault-tolerant upstream access and order-level authorization.
package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"orderguard/internal/auth"
	"orderguard/internal/observability"
	"orderguard/internal/provider"
	"orderguard/internal/resilience"
)

// Config tunes the resilience envelope around the provider.
type Config struct {
	AttemptTimeout   time.Duration // deadline for a single upstream call
	RequestTimeout   time.Duration // deadline for all attempts of one lookup
	Retry            resilience.RetryPolicy
	BreakerThreshold int           // consecutive failures that open the circuit
	BreakerCooldown  time.Duration // time the circuit stays open
	MaxConcurrent    int           // bulkhead size for upstream calls
	BulkheadWait     time.Duration // max wait for a bulkhead slot
	StaleTTL         time.Duration // max age of last-known-good data; 0 disables
	StaleCacheSize   int
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		AttemptTimeout:   2 * time.Second,
		RequestTimeout:   5 * time.Second,
		Retry:            resilience.RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, Jitter: true},
		BreakerThreshold: 5,
		BreakerCooldown:  10 * time.Second,
		MaxConcurrent:    32,
		BulkheadWait:     250 * time.Millisecond,
		StaleTTL:         10 * time.Minute,
		StaleCacheSize:   1000,
	}
}

// Source says where a Result came from.
type Source string

const (
	SourceLive  Source = "live"
	SourceStale Source = "stale"
)

// Result is a successful lookup.
type Result struct {
	Order     *provider.Order
	Source    Source
	FetchedAt time.Time
}

// Health is a snapshot of dependency health.
type Health struct {
	Provider        string
	BreakerState    resilience.State
	UpstreamInUse   int
	UpstreamLimit   int
	StaleCacheItems int
}

// Service looks up orders on behalf of authenticated principals.
type Service struct {
	provider provider.Provider
	cfg      Config
	log      *slog.Logger
	metrics  *observability.Metrics

	retrier  resilience.Retrier
	breaker  *resilience.Breaker
	bulkhead *resilience.Bulkhead
	group    resilience.Group[string, fetched]
	cache    *staleCache[*provider.Order]
	lists    *staleCache[[]*provider.Order]
	now      func() time.Time
}

// fetched is the result of one coalesced upstream call: a single order or a list.
type fetched struct {
	order  *provider.Order
	orders []*provider.Order
	at     time.Time
}

// NewService wires a provider into the resilience envelope.
func NewService(p provider.Provider, cfg Config, log *slog.Logger, m *observability.Metrics) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if m == nil {
		m = observability.NewMetrics()
	}
	s := &Service{
		provider: p,
		cfg:      cfg,
		log:      log.With("component", "orders", "provider", p.Name()),
		metrics:  m,
		bulkhead: resilience.NewBulkhead(cfg.MaxConcurrent, cfg.BulkheadWait),
		cache:    newStaleCache[*provider.Order](cfg.StaleTTL, cfg.StaleCacheSize),
		lists:    newStaleCache[[]*provider.Order](cfg.StaleTTL, cfg.StaleCacheSize),
		now:      time.Now,
	}
	s.retrier = resilience.Retrier{
		Policy:      cfg.Retry,
		ShouldRetry: isTransient,
		RetryAfter:  provider.RetryAfter,
		OnRetry: func(attempt int, delay time.Duration, err error) {
			m.UpstreamRetries.Inc(p.Name())
			s.log.Warn("retrying upstream call", "attempt", attempt, "delay_ms", delay.Milliseconds(), "error", err)
		},
	}
	breakerName := p.Name()
	s.breaker = resilience.NewBreaker(resilience.BreakerConfig{
		Name:             breakerName,
		FailureThreshold: cfg.BreakerThreshold,
		OpenTimeout:      cfg.BreakerCooldown,
		IsFailure:        isTransient,
		OnStateChange: func(name string, from, to resilience.State) {
			m.BreakerState.Set(float64(to), name)
			m.BreakerTransitions.Inc(name, to.String())
			s.log.Warn("circuit breaker state change", "breaker", name, "from", from.String(), "to", to.String())
		},
	})
	m.BreakerState.Set(float64(resilience.StateClosed), breakerName)
	return s
}

// isTransient reports whether err signals a temporary upstream problem.
// It drives both retry decisions and circuit-breaker failure counting.
func isTransient(err error) bool {
	if provider.IsRetryable(err) {
		return true
	}
	// A raw deadline error from a provider means our per-attempt timeout fired.
	return errors.Is(err, context.DeadlineExceeded) && provider.KindOf(err) == provider.KindUnknown
}

var orderIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// ValidOrderID reports whether id is a well-formed numeric order ID.
func ValidOrderID(id string) bool {
	if !orderIDPattern.MatchString(id) {
		return false
	}
	_, err := strconv.ParseUint(id, 10, 64)
	return err == nil
}

// GetOrder returns the order if it exists and p may see it. Orders the
// caller is not authorized for are reported as not found, so order IDs
// cannot be enumerated.
func (s *Service) GetOrder(ctx context.Context, p auth.Principal, id string) (*Result, error) {
	if !ValidOrderID(id) {
		return nil, &Error{Kind: KindInvalidID}
	}

	res, err := s.lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	if !auth.CanAccessOrder(p, res.Order.CustomerID) {
		s.metrics.AuthzDenied.Inc()
		s.log.WarnContext(ctx, "order access denied", "principal", p.ID, "role", string(p.Role), "order_id", id)
		return nil, &Error{Kind: KindNotFound, Err: errors.New("principal not authorized for order")}
	}
	return res, nil
}

func (s *Service) lookup(ctx context.Context, id string) (*Result, error) {
	f, err, shared := s.group.Do(ctx, id, func(ctx context.Context) (fetched, error) {
		return s.fetch(ctx, id)
	})
	if shared {
		s.metrics.Deduplicated.Inc()
	}
	if err == nil {
		return &Result{Order: f.order, Source: SourceLive, FetchedAt: f.at}, nil
	}

	oerr := s.classify(ctx, err)
	switch oerr.Kind {
	case KindUnavailable, KindTimeout:
		if order, at, ok := s.lastKnown(id); ok {
			s.metrics.StaleServed.Inc()
			s.log.WarnContext(ctx, "serving stale order after upstream failure",
				"order_id", id, "age_s", int(s.now().Sub(at).Seconds()), "error", err)
			return &Result{Order: order, Source: SourceStale, FetchedAt: at}, nil
		}
	case KindNotFound:
		s.cache.remove(id)
	}
	return nil, oerr
}

// lastKnown finds the newest saved copy of an order: from a single lookup or
// from a saved order list (e.g. a search). Authorization is checked by the caller.
func (s *Service) lastKnown(id string) (*provider.Order, time.Time, bool) {
	var (
		best *provider.Order
		at   time.Time
	)
	if e, ok := s.cache.get(id); ok {
		best, at = e.value, e.fetchedAt
	}
	s.lists.each(func(e cacheEntry[[]*provider.Order]) bool {
		if !e.fetchedAt.After(at) {
			return true
		}
		for _, o := range e.value {
			if o.ID == id {
				best, at = o, e.fetchedAt
				break
			}
		}
		return true
	})
	return best, at, best != nil
}

// fetch runs on a context detached from the caller (see resilience.Group),
// so it owns the overall deadline.
func (s *Service) fetch(ctx context.Context, id string) (fetched, error) {
	var order *provider.Order
	err := s.protect(ctx, "order_id", id, func(actx context.Context) error {
		o, err := s.provider.GetOrder(actx, id)
		if err == nil && o == nil {
			err = &provider.Error{Kind: provider.KindBadResponse, Op: "orders.fetch", Err: errors.New("provider returned nil order")}
		}
		order = o
		return err
	})
	if err != nil {
		return fetched{}, err
	}
	at := s.now()
	s.cache.put(id, order, at)
	return fetched{order: order, at: at}, nil
}

// fetchList loads a customer's recent orders ("" for all orders).
func (s *Service) fetchList(ctx context.Context, customerID string) (fetched, error) {
	var orders []*provider.Order
	err := s.protect(ctx, "customer_id", customerID, func(actx context.Context) error {
		list, err := s.provider.ListOrders(actx, provider.ListFilter{CustomerID: customerID, Limit: provider.MaxListLimit})
		orders = list
		return err
	})
	if err != nil {
		return fetched{}, err
	}
	at := s.now()
	s.lists.put(customerID, orders, at)
	return fetched{orders: orders, at: at}, nil
}

// protect runs one logical upstream operation inside the resilience
// envelope: overall deadline, bounded retries, bulkhead, circuit breaker and
// a per-attempt timeout, with metrics for every attempt.
func (s *Service) protect(ctx context.Context, logKey, logValue string, call func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()

	return s.retrier.Do(ctx, func(ctx context.Context, attempt int) error {
		release, err := s.bulkhead.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()

		return s.breaker.Execute(func() error {
			actx, cancel := context.WithTimeout(ctx, s.cfg.AttemptTimeout)
			defer cancel()

			start := time.Now()
			err := call(actx)
			s.metrics.UpstreamDuration.Observe(time.Since(start).Seconds(), s.provider.Name())
			s.metrics.UpstreamRequests.Inc(s.provider.Name(), outcome(err))
			if err != nil {
				s.log.DebugContext(ctx, "upstream call failed", "attempt", attempt, logKey, logValue, "error", err)
			}
			return err
		})
	})
}

func outcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.DeadlineExceeded) && provider.KindOf(err) == provider.KindUnknown:
		return provider.KindTimeout.String()
	}
	return provider.KindOf(err).String()
}

// classify converts internal errors into client-facing ones.
func (s *Service) classify(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		return &Error{Kind: KindCanceled, Err: err}
	}
	switch {
	case errors.Is(err, resilience.ErrCircuitOpen):
		return &Error{Kind: KindUnavailable, RetryAfter: max(s.breaker.RetryAfter(), time.Second), Err: err}
	case errors.Is(err, resilience.ErrBulkheadFull):
		return &Error{Kind: KindUnavailable, RetryAfter: time.Second, Err: err}
	}

	switch provider.KindOf(err) {
	case provider.KindNotFound:
		return &Error{Kind: KindNotFound, Err: err}
	case provider.KindRateLimited, provider.KindUnavailable:
		ra, _ := provider.RetryAfter(err)
		return &Error{Kind: KindUnavailable, RetryAfter: max(ra, time.Second), Err: err}
	case provider.KindTimeout:
		return &Error{Kind: KindTimeout, Err: err}
	case provider.KindBadResponse, provider.KindUnauthorized:
		return &Error{Kind: KindUpstream, Err: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: KindTimeout, Err: err}
	}
	return &Error{Kind: KindInternal, Err: fmt.Errorf("unexpected provider error: %w", err)}
}

// MaxQueryLength bounds search patterns. Go's regexp is RE2 (linear time),
// so a long pattern cannot cause catastrophic backtracking; the bound just
// keeps compiled programs small.
const MaxQueryLength = 200

// Match is one search hit. MatchedOn lists what matched: "order_number" and/or "items".
type Match struct {
	Order     *provider.Order
	MatchedOn []string
}

// SearchResult is the outcome of SearchOrders.
type SearchResult struct {
	Matches   []Match
	Total     int // matches before the limit was applied
	Source    Source
	FetchedAt time.Time
}

// SearchOrders matches a case-insensitive regular expression against the
// order number and the titles of ordered items, over the orders p may see:
// a customer's own orders, or every order for support. An empty query lists
// those orders. Results are newest first.
func (s *Service) SearchOrders(ctx context.Context, p auth.Principal, query string, limit int) (*SearchResult, error) {
	if len(query) > MaxQueryLength {
		return nil, &Error{Kind: KindInvalidQuery, Err: fmt.Errorf("query longer than %d characters", MaxQueryLength)}
	}
	var re *regexp.Regexp
	if query != "" {
		var err error
		if re, err = regexp.Compile("(?i)" + query); err != nil {
			return nil, &Error{Kind: KindInvalidQuery, Err: err}
		}
	}
	if limit <= 0 || limit > provider.MaxListLimit {
		limit = 10
	}

	var scope string
	switch p.Role {
	case auth.RoleSupport:
	case auth.RoleCustomer:
		if p.CustomerID == "" {
			return &SearchResult{Source: SourceLive, FetchedAt: s.now()}, nil
		}
		scope = p.CustomerID
	default:
		return &SearchResult{Source: SourceLive, FetchedAt: s.now()}, nil
	}

	f, err, shared := s.group.Do(ctx, "list:"+scope, func(ctx context.Context) (fetched, error) {
		return s.fetchList(ctx, scope)
	})
	if shared {
		s.metrics.Deduplicated.Inc()
	}
	source := SourceLive
	if err != nil {
		oerr := s.classify(ctx, err)
		e, ok := s.lists.get(scope)
		if (oerr.Kind != KindUnavailable && oerr.Kind != KindTimeout) || !ok {
			return nil, oerr
		}
		s.metrics.StaleServed.Inc()
		s.log.WarnContext(ctx, "serving stale order list after upstream failure",
			"age_s", int(s.now().Sub(e.fetchedAt).Seconds()), "error", err)
		f, source = fetched{orders: e.value, at: e.fetchedAt}, SourceStale
	}

	res := &SearchResult{Source: source, FetchedAt: f.at}
	for _, o := range f.orders {
		// Defence in depth: never rely on the upstream filter alone.
		if !auth.CanAccessOrder(p, o.CustomerID) {
			continue
		}
		var on []string
		if re != nil {
			if re.MatchString(o.Name) || re.MatchString(o.ID) {
				on = append(on, "order_number")
			}
			for _, li := range o.LineItems {
				if re.MatchString(li.Title) {
					on = append(on, "items")
					break
				}
			}
			if len(on) == 0 {
				continue
			}
		}
		res.Total++
		if len(res.Matches) < limit {
			res.Matches = append(res.Matches, Match{Order: o, MatchedOn: on})
		}
	}
	return res, nil
}

// Health returns a snapshot for the health endpoint.
func (s *Service) Health() Health {
	return Health{
		Provider:        s.provider.Name(),
		BreakerState:    s.breaker.State(),
		UpstreamInUse:   s.bulkhead.InUse(),
		UpstreamLimit:   s.bulkhead.Capacity(),
		StaleCacheItems: s.cache.len() + s.lists.len(),
	}
}

// PurgeCache drops all last-known-good copies. The interactive demo uses it
// so failure scenarios behave the same for every visitor.
func (s *Service) PurgeCache() {
	s.cache.purge()
	s.lists.purge()
}
