package orders

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orderguard/internal/auth"
	"orderguard/internal/observability"
	"orderguard/internal/provider"
	"orderguard/internal/provider/fixtures"
	"orderguard/internal/resilience"
	"slices"
	"strings"
)

// scriptedProvider returns queued errors before falling back to fixtures.
type scriptedProvider struct {
	mu      sync.Mutex
	errs    []error
	always  error
	delay   time.Duration
	calls   atomic.Int64
	fixture map[string]*provider.Order
}

func newScripted() *scriptedProvider {
	p := &scriptedProvider{fixture: map[string]*provider.Order{}}
	for _, o := range fixtures.Orders() {
		p.fixture[o.ID] = &o
	}
	return p
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) GetOrder(ctx context.Context, id string) (*provider.Order, error) {
	p.calls.Add(1)
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	var err error
	if len(p.errs) > 0 {
		err, p.errs = p.errs[0], p.errs[1:]
	} else {
		err = p.always
	}
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	o, ok := p.fixture[id]
	if !ok {
		return nil, provider.NotFound("scripted")
	}
	return o, nil
}

func (p *scriptedProvider) ListOrders(ctx context.Context, f provider.ListFilter) ([]*provider.Order, error) {
	if _, err := p.GetOrder(ctx, "1001"); err != nil && provider.KindOf(err) != provider.KindNotFound {
		return nil, err // same scripted failures and latency as GetOrder
	}
	var out []*provider.Order
	for _, o := range fixtures.Orders() {
		if f.CustomerID == "" || o.CustomerID == f.CustomerID {
			out = append(out, p.fixture[o.ID])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (p *scriptedProvider) setAlways(err error) {
	p.mu.Lock()
	p.always = err
	p.mu.Unlock()
}

var (
	alice   = auth.Principal{ID: "alice", Role: auth.RoleCustomer, CustomerID: fixtures.CustomerAlice}
	bob     = auth.Principal{ID: "bob", Role: auth.RoleCustomer, CustomerID: fixtures.CustomerBob}
	support = auth.Principal{ID: "support", Role: auth.RoleSupport}

	unavailable = &provider.Error{Kind: provider.KindUnavailable, Op: "test", StatusCode: 503}
)

func testConfig() Config {
	return Config{
		AttemptTimeout:   100 * time.Millisecond,
		RequestTimeout:   time.Second,
		Retry:            resilience.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond},
		BreakerThreshold: 5,
		BreakerCooldown:  time.Minute,
		MaxConcurrent:    8,
		BulkheadWait:     50 * time.Millisecond,
		StaleTTL:         time.Minute,
		StaleCacheSize:   10,
	}
}

func newTestService(p provider.Provider, cfg Config) (*Service, *observability.Metrics) {
	m := observability.NewMetrics()
	return NewService(p, cfg, nil, m), m
}

func wantKind(t *testing.T, err error, k Kind) {
	t.Helper()
	if got := KindOf(err); got != k {
		t.Fatalf("kind = %v, want %v (err: %v)", got, k, err)
	}
}

func TestGetOrderAuthorization(t *testing.T) {
	svc, m := newTestService(newScripted(), testConfig())
	ctx := context.Background()

	res, err := svc.GetOrder(ctx, alice, "1001")
	if err != nil || res.Order.ID != "1001" || res.Source != SourceLive {
		t.Fatalf("owner lookup = %+v, %v", res, err)
	}
	_, err = svc.GetOrder(ctx, bob, "1001")
	wantKind(t, err, KindNotFound)
	if m.AuthzDenied.Value() != 1 {
		t.Fatalf("authz denied metric = %v", m.AuthzDenied.Value())
	}
	if _, err := svc.GetOrder(ctx, support, "1001"); err != nil {
		t.Fatalf("support lookup: %v", err)
	}
	_, err = svc.GetOrder(ctx, alice, "1004") // guest order
	wantKind(t, err, KindNotFound)
}

func TestGetOrderValidatesID(t *testing.T) {
	p := newScripted()
	svc, _ := newTestService(p, testConfig())
	for _, id := range []string{"", "0", "-1", "abc", "1 OR 1=1", "../1", "123456789012345678901", "99999999999999999999"} {
		_, err := svc.GetOrder(context.Background(), support, id)
		wantKind(t, err, KindInvalidID)
	}
	if p.calls.Load() != 0 {
		t.Fatal("invalid IDs must not reach the provider")
	}
}

func TestGetOrderNotFound(t *testing.T) {
	svc, _ := newTestService(newScripted(), testConfig())
	_, err := svc.GetOrder(context.Background(), support, "999")
	wantKind(t, err, KindNotFound)
}

func TestGetOrderRetriesTransientFailures(t *testing.T) {
	p := newScripted()
	p.errs = []error{unavailable, &provider.Error{Kind: provider.KindTimeout, Op: "test"}}
	svc, m := newTestService(p, testConfig())

	if _, err := svc.GetOrder(context.Background(), alice, "1001"); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 3 {
		t.Fatalf("provider calls = %d, want 3", p.calls.Load())
	}
	if got := m.UpstreamRetries.Value("scripted"); got != 2 {
		t.Fatalf("retries metric = %v, want 2", got)
	}
}

func TestGetOrderDoesNotRetryPermanentFailures(t *testing.T) {
	p := newScripted()
	p.errs = []error{&provider.Error{Kind: provider.KindBadResponse, Op: "test"}}
	svc, _ := newTestService(p, testConfig())
	_, err := svc.GetOrder(context.Background(), alice, "1001")
	wantKind(t, err, KindUpstream)
	if p.calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls.Load())
	}
}

func TestGetOrderExhaustedRetriesIsUnavailable(t *testing.T) {
	p := newScripted()
	p.always = unavailable
	svc, _ := newTestService(p, testConfig())
	_, err := svc.GetOrder(context.Background(), alice, "1001")
	wantKind(t, err, KindUnavailable)
	var oe *Error
	if !errors.As(err, &oe) || oe.RetryAfter < time.Second {
		t.Fatalf("RetryAfter = %v, want >= 1s", oe.RetryAfter)
	}
	if p.calls.Load() != 3 {
		t.Fatalf("provider calls = %d, want 3", p.calls.Load())
	}
}

func TestGetOrderAttemptTimeout(t *testing.T) {
	p := newScripted()
	p.delay = time.Second // longer than AttemptTimeout
	cfg := testConfig()
	cfg.Retry.MaxAttempts = 2
	svc, _ := newTestService(p, cfg)

	start := time.Now()
	_, err := svc.GetOrder(context.Background(), alice, "1001")
	wantKind(t, err, KindTimeout)
	if p.calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2 (timeouts are retryable)", p.calls.Load())
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("took %v; attempt timeout not enforced", elapsed)
	}
}

func TestGetOrderRequestBudget(t *testing.T) {
	p := newScripted()
	p.delay = time.Second
	cfg := testConfig()
	cfg.AttemptTimeout = 80 * time.Millisecond
	cfg.RequestTimeout = 150 * time.Millisecond
	cfg.Retry.MaxAttempts = 10
	svc, _ := newTestService(p, cfg)

	start := time.Now()
	_, err := svc.GetOrder(context.Background(), alice, "1001")
	wantKind(t, err, KindTimeout)
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("took %v; overall budget not enforced", elapsed)
	}
	if p.calls.Load() > 2 {
		t.Fatalf("provider calls = %d; retries exceeded the request budget", p.calls.Load())
	}
}

func TestCircuitBreakerOpensAndFailsFast(t *testing.T) {
	p := newScripted()
	p.always = unavailable
	cfg := testConfig()
	cfg.BreakerThreshold = 3
	cfg.Retry.MaxAttempts = 1
	cfg.StaleTTL = 0
	svc, m := newTestService(p, cfg)

	for range 3 {
		_, _ = svc.GetOrder(context.Background(), alice, "1001")
	}
	if svc.Health().BreakerState != resilience.StateOpen {
		t.Fatalf("breaker = %v, want open", svc.Health().BreakerState)
	}
	callsBefore := p.calls.Load()
	_, err := svc.GetOrder(context.Background(), alice, "1001")
	wantKind(t, err, KindUnavailable)
	if p.calls.Load() != callsBefore {
		t.Fatal("open breaker must not call the provider")
	}
	if m.BreakerState.Value("scripted") != float64(resilience.StateOpen) {
		t.Fatal("breaker state metric not updated")
	}
}

func TestStaleFallbackDuringOutage(t *testing.T) {
	p := newScripted()
	svc, m := newTestService(p, testConfig())
	ctx := context.Background()

	live, err := svc.GetOrder(ctx, alice, "1001")
	if err != nil {
		t.Fatal(err)
	}
	p.setAlways(unavailable)

	res, err := svc.GetOrder(ctx, alice, "1001")
	if err != nil {
		t.Fatalf("expected stale fallback, got %v", err)
	}
	if res.Source != SourceStale || !res.FetchedAt.Equal(live.FetchedAt) {
		t.Fatalf("result = %+v, want stale copy fetched at %v", res, live.FetchedAt)
	}
	if m.StaleServed.Value() != 1 {
		t.Fatal("stale metric not incremented")
	}

	// Authorization still applies to cached data.
	_, err = svc.GetOrder(ctx, bob, "1001")
	wantKind(t, err, KindNotFound)

	// Nothing cached for an order never fetched successfully.
	_, err = svc.GetOrder(ctx, alice, "1003")
	wantKind(t, err, KindUnavailable)
}

func TestStaleEntriesExpire(t *testing.T) {
	p := newScripted()
	svc, _ := newTestService(p, testConfig())
	now := time.Now()
	svc.cache.now = func() time.Time { return now }

	if _, err := svc.GetOrder(context.Background(), alice, "1001"); err != nil {
		t.Fatal(err)
	}
	p.setAlways(unavailable)
	now = now.Add(2 * time.Minute) // beyond StaleTTL
	_, err := svc.GetOrder(context.Background(), alice, "1001")
	wantKind(t, err, KindUnavailable)
}

func TestConcurrentLookupsAreCoalesced(t *testing.T) {
	p := newScripted()
	p.delay = 50 * time.Millisecond
	svc, m := newTestService(p, testConfig())

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			who := alice
			if i%2 == 0 {
				who = support
			}
			_, err := svc.GetOrder(context.Background(), who, "1001")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if c := p.calls.Load(); c > 3 {
		t.Fatalf("provider calls = %d for %d concurrent lookups; expected coalescing", c, n)
	}
	if m.Deduplicated.Value() == 0 {
		t.Fatal("deduplication metric not incremented")
	}
}

func TestCallerCancellation(t *testing.T) {
	p := newScripted()
	p.delay = 200 * time.Millisecond
	svc, _ := newTestService(p, testConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := svc.GetOrder(ctx, alice, "1001")
	wantKind(t, err, KindCanceled)
}

func TestBulkheadRejectsExcessConcurrency(t *testing.T) {
	p := newScripted()
	p.delay = 200 * time.Millisecond
	cfg := testConfig()
	cfg.MaxConcurrent = 2
	cfg.BulkheadWait = 10 * time.Millisecond
	cfg.AttemptTimeout = time.Second
	svc, _ := newTestService(p, cfg)

	var wg sync.WaitGroup
	var unavailableCount atomic.Int64
	for _, id := range []string{"1001", "1002", "1003", "1004"} { // distinct IDs defeat coalescing
		wg.Go(func() {
			_, err := svc.GetOrder(context.Background(), support, id)
			if KindOf(err) == KindUnavailable {
				unavailableCount.Add(1)
			}
		})
	}
	wg.Wait()
	if unavailableCount.Load() != 2 {
		t.Fatalf("unavailable = %d, want 2 rejected by the bulkhead", unavailableCount.Load())
	}
}

func TestStaleCacheEvictsLRU(t *testing.T) {
	c := newStaleCache[*provider.Order](time.Minute, 2)
	o := &provider.Order{}
	now := time.Now()
	c.put("a", o, now)
	c.put("b", o, now)
	c.get("a")
	c.put("a", o, now) // refresh a
	c.put("c", o, now) // evicts b
	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a should be present")
	}
	if c.len() != 2 {
		t.Fatalf("len = %d", c.len())
	}
	var disabled *staleCache[*provider.Order]
	disabled.put("x", o, now)
	if _, ok := disabled.get("x"); ok || disabled.len() != 0 {
		t.Fatal("nil cache must be a no-op")
	}
}

func ids(res *SearchResult) []string {
	out := make([]string, 0, len(res.Matches))
	for _, m := range res.Matches {
		out = append(out, m.Order.ID)
	}
	return out
}

func TestSearchOrdersMatchesNumberAndItems(t *testing.T) {
	svc, _ := newTestService(newScripted(), testConfig())
	ctx := context.Background()

	cases := []struct {
		who   auth.Principal
		query string
		want  []string
		on    string
	}{
		{bob, "water bottle", []string{"1002"}, "items"},
		{bob, "BOTTLE", []string{"1002"}, "items"},           // case-insensitive
		{alice, "jacket|shoe", []string{"1003", "1001"}, ""}, // newest first
		{alice, "^#100[13]$", []string{"1003", "1001"}, "order_number"},
		{alice, "1001", []string{"1001"}, "order_number"},
		{alice, "", []string{"1003", "1001"}, ""}, // empty query lists the caller's orders
		{alice, "water bottle", []string{}, ""},   // Bob's item is invisible to Alice
		{support, "gift card", []string{"1004"}, "items"},
		{support, "", []string{"1004", "1003", "1002", "1001"}, ""},
	}
	for _, c := range cases {
		res, err := svc.SearchOrders(ctx, c.who, c.query, 0)
		if err != nil {
			t.Fatalf("%s %q: %v", c.who.ID, c.query, err)
		}
		got := ids(res)
		if strings.Join(got, ",") != strings.Join(c.want, ",") || res.Total != len(c.want) {
			t.Errorf("%s %q = %v (total %d), want %v", c.who.ID, c.query, got, res.Total, c.want)
		}
		if c.on != "" && len(res.Matches) > 0 && !slices.Contains(res.Matches[0].MatchedOn, c.on) {
			t.Errorf("%s %q matched on %v, want %s", c.who.ID, c.query, res.Matches[0].MatchedOn, c.on)
		}
	}
}

func TestSearchOrdersValidationAndLimit(t *testing.T) {
	p := newScripted()
	svc, _ := newTestService(p, testConfig())
	for _, q := range []string{"(unclosed", "[a-", strings.Repeat("a", MaxQueryLength+1)} {
		_, err := svc.SearchOrders(context.Background(), alice, q, 0)
		wantKind(t, err, KindInvalidQuery)
	}
	if p.calls.Load() != 0 {
		t.Fatal("invalid queries must not reach the provider")
	}
	res, err := svc.SearchOrders(context.Background(), support, "", 2)
	if err != nil || len(res.Matches) != 2 || res.Total != 4 {
		t.Fatalf("limit: %v matches, total %d, err %v", len(res.Matches), res.Total, err)
	}
	res, _ = svc.SearchOrders(context.Background(), auth.Principal{ID: "x", Role: "unknown"}, "", 0)
	if len(res.Matches) != 0 {
		t.Fatal("unknown role must see nothing")
	}
}

func TestSearchOrdersResilience(t *testing.T) {
	p := newScripted()
	p.errs = []error{unavailable}
	svc, _ := newTestService(p, testConfig())
	ctx := context.Background()

	res, err := svc.SearchOrders(ctx, bob, "bottle", 0)
	if err != nil || res.Source != SourceLive || len(res.Matches) != 1 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	p.setAlways(unavailable)
	res, err = svc.SearchOrders(ctx, bob, "bottle", 0)
	if err != nil || res.Source != SourceStale || len(res.Matches) != 1 {
		t.Fatalf("stale fallback: %+v %v", res, err)
	}
	_, err = svc.SearchOrders(ctx, alice, "", 0) // nothing cached for Alice
	wantKind(t, err, KindUnavailable)
}

func TestStaleLookupUsesSavedSearchResults(t *testing.T) {
	p := newScripted()
	svc, _ := newTestService(p, testConfig())
	ctx := context.Background()
	if _, err := svc.SearchOrders(ctx, bob, "bottle", 0); err != nil {
		t.Fatal(err)
	}
	p.setAlways(unavailable)
	res, err := svc.GetOrder(ctx, bob, "1002") // never looked up on its own
	if err != nil || res.Source != SourceStale || res.Order.ID != "1002" {
		t.Fatalf("got %+v, %v", res, err)
	}
	_, err = svc.GetOrder(ctx, alice, "1002") // still authorized per caller
	wantKind(t, err, KindNotFound)
}
