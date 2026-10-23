package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orderguard/internal/api"
	"orderguard/internal/orders"
	"orderguard/internal/provider/shopify/shopifytest"
	"orderguard/internal/resilience"
)

// Demo 1: successful order retrieval.
func TestSuccessfulOrderRetrieval(t *testing.T) {
	s := newStack(t)
	r := s.get("alice", "/orders/1001")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", r.status, r.body)
	}
	o := r.order(t)
	if o.Order.Name != "#1001" || o.Order.FulfillmentStatus != "partially_fulfilled" ||
		o.Order.FinancialStatus != "paid" || o.Meta.Source != "live" {
		t.Fatalf("order = %+v", o)
	}
	if len(o.Order.LineItems) != 2 || o.Order.Fulfillments[0].Tracking[0].Company != "Example Parcel" {
		t.Fatalf("details = %+v", o.Order)
	}

	r = s.get("alice", "/orders/1001/fulfillment")
	if r.status != http.StatusOK {
		t.Fatalf("fulfillment status = %d", r.status)
	}
}

// Demo 2: unauthorized order access.
func TestUnauthorizedOrderAccess(t *testing.T) {
	s := newStack(t)

	if r := s.get("nobody", "/orders/1001"); r.status != http.StatusUnauthorized || r.errorCode(t) != "unauthorized" {
		t.Fatalf("no key: status = %d", r.status)
	}

	// Bob may not see Alice's order; the response is indistinguishable from
	// a missing order so IDs cannot be enumerated.
	denied := s.get("bob", "/orders/1001")
	missing := s.get("bob", "/orders/987654")
	if denied.status != http.StatusNotFound || denied.errorCode(t) != "order_not_found" {
		t.Fatalf("cross-customer: status = %d, body = %s", denied.status, denied.body)
	}
	if missing.status != denied.status || missing.errorCode(t) != denied.errorCode(t) {
		t.Fatal("denied and missing responses differ")
	}
	if s.metrics.AuthzDenied.Value() != 1 {
		t.Fatalf("authz denied metric = %v", s.metrics.AuthzDenied.Value())
	}

	// Guest orders are not visible to customer keys, but are to support.
	if r := s.get("alice", "/orders/1004"); r.status != http.StatusNotFound {
		t.Fatalf("guest order via customer key: %d", r.status)
	}
	if r := s.get("support-bot", "/orders/1004"); r.status != http.StatusOK {
		t.Fatalf("guest order via support key: %d", r.status)
	}

	if r := s.get("alice", "/orders/not-a-number"); r.status != http.StatusBadRequest || r.errorCode(t) != "invalid_order_id" {
		t.Fatalf("invalid id: %d", r.status)
	}
}

// Demo 3: upstream API timeout.
func TestUpstreamTimeout(t *testing.T) {
	s := newStack(t, func(o *orders.Config, _ *api.Config) { o.StaleTTL = 0 })
	s.shopify.SetFaults(shopifytest.Faults{Latency: 2 * time.Second})

	start := time.Now()
	r := s.get("alice", "/orders/1001")
	elapsed := time.Since(start)

	if r.status != http.StatusGatewayTimeout || r.errorCode(t) != "upstream_timeout" {
		t.Fatalf("status = %d, body = %s", r.status, r.body)
	}
	// 3 attempts × 200ms + backoff, well under the 2s upstream latency.
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("request took %v; timeouts not enforced", elapsed)
	}
	if got := s.shopify.Requests(); got != 3 {
		t.Fatalf("upstream requests = %d, want 3 (timeouts are retried)", got)
	}
}

// Demo 4: retry behaviour.
func TestRetryRecoversFromTransientFailures(t *testing.T) {
	s := newStack(t)
	s.shopify.SetFaults(shopifytest.Faults{FailFirst: 2, FailStatus: http.StatusBadGateway})

	r := s.get("alice", "/orders/1001")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", r.status, r.body)
	}
	if got := s.shopify.Requests(); got != 3 {
		t.Fatalf("upstream requests = %d, want 3", got)
	}
	if got := s.metrics.UpstreamRetries.Value("shopify"); got != 2 {
		t.Fatalf("retry metric = %v, want 2", got)
	}
}

func TestRetryGivesUpWithinBudget(t *testing.T) {
	s := newStack(t, func(o *orders.Config, _ *api.Config) { o.StaleTTL = 0 })
	s.shopify.SetFaults(shopifytest.Faults{FailRate: 1})

	r := s.get("alice", "/orders/1001")
	if r.status != http.StatusServiceUnavailable || r.errorCode(t) != "upstream_unavailable" || r.header.Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q", r.status, r.header.Get("Retry-After"))
	}
	if got := s.shopify.Requests(); got != 3 {
		t.Fatalf("upstream requests = %d, want exactly 3 (bounded retries)", got)
	}
}

func TestShopifyThrottlingIsRetriedAfterDelay(t *testing.T) {
	s := newStack(t, func(o *orders.Config, _ *api.Config) { o.RequestTimeout = 3 * time.Second })
	s.shopify.SetFaults(shopifytest.Faults{Throttle: true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.shopify.SetFaults(shopifytest.Faults{})
	}()

	start := time.Now()
	r := s.get("alice", "/orders/1001")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", r.status, r.body)
	}
	// The fake reports a 1s refill time; the retry must wait for it rather
	// than hammering the API with the 10ms base backoff.
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retried after %v, ignoring the throttle delay", elapsed)
	}
}

func TestNonRetryableUpstreamErrorsFailFast(t *testing.T) {
	s := newStack(t, func(o *orders.Config, _ *api.Config) { o.StaleTTL = 0 })
	s.shopify.SetFaults(shopifytest.Faults{MalformedResponses: true})
	r := s.get("alice", "/orders/1001")
	if r.status != http.StatusBadGateway || r.errorCode(t) != "upstream_error" {
		t.Fatalf("status = %d", r.status)
	}
	if s.shopify.Requests() != 1 {
		t.Fatalf("upstream requests = %d, want 1", s.shopify.Requests())
	}
}

// Demo 5: concurrent requests.
func TestConcurrentRequests(t *testing.T) {
	s := newStack(t)
	s.shopify.SetFaults(shopifytest.Faults{Latency: 20 * time.Millisecond})

	type call struct {
		who, id string
		want    int
	}
	calls := []call{
		{"alice", "1001", 200}, {"alice", "1003", 200}, {"bob", "1002", 200},
		{"bob", "1001", 404}, {"alice", "1002", 404}, {"support-bot", "1004", 200},
	}

	const workers, perWorker = 50, 20
	var wg sync.WaitGroup
	var failures atomic.Int64
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				c := calls[(w+i)%len(calls)]
				r := s.get(c.who, "/orders/"+c.id)
				if r.status != c.want {
					failures.Add(1)
					t.Errorf("%s GET %s = %d, want %d", c.who, c.id, r.status, c.want)
					continue
				}
				if c.want == 200 {
					if o := r.order(t); o.Order.ID != c.id {
						failures.Add(1)
						t.Errorf("%s GET %s returned order %s", c.who, c.id, o.Order.ID)
					}
				}
			}
		})
	}
	wg.Wait()

	total := workers * perWorker
	upstream := s.shopify.Requests()
	t.Logf("%d client requests -> %d upstream requests (%d coalesced)", total, upstream, int64(s.metrics.Deduplicated.Value()))
	if failures.Load() > 0 {
		t.FailNow()
	}
	if upstream >= int64(total) {
		t.Fatalf("upstream requests = %d for %d client requests; coalescing ineffective", upstream, total)
	}
}

func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	s := newStack(t, func(o *orders.Config, _ *api.Config) {
		o.BreakerThreshold = 3
		o.BreakerCooldown = 200 * time.Millisecond
		o.Retry.MaxAttempts = 1
		o.StaleTTL = 0
	})
	s.shopify.SetFaults(shopifytest.Faults{FailRate: 1})

	for range 3 {
		s.get("alice", "/orders/1001")
	}
	if s.svc.Health().BreakerState != resilience.StateOpen {
		t.Fatalf("breaker = %v, want open", s.svc.Health().BreakerState)
	}

	before := s.shopify.Requests()
	start := time.Now()
	r := s.get("alice", "/orders/1001")
	if r.status != http.StatusServiceUnavailable || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("open breaker: status = %d after %v", r.status, time.Since(start))
	}
	if s.shopify.Requests() != before {
		t.Fatal("open breaker forwarded a request upstream")
	}
	if h := s.get("", "/health"); !strings.Contains(string(h.body), `"degraded"`) {
		t.Fatalf("health = %s", h.body)
	}

	s.shopify.SetFaults(shopifytest.Faults{})
	time.Sleep(250 * time.Millisecond)
	if r := s.get("alice", "/orders/1001"); r.status != http.StatusOK {
		t.Fatalf("after recovery: status = %d", r.status)
	}
	if s.svc.Health().BreakerState != resilience.StateClosed {
		t.Fatal("breaker did not close after a successful probe")
	}
}

func TestGracefulDegradationServesStaleData(t *testing.T) {
	s := newStack(t)
	if r := s.get("alice", "/orders/1001"); r.status != http.StatusOK {
		t.Fatal(r.status)
	}
	s.shopify.SetFaults(shopifytest.Faults{FailRate: 1})

	r := s.get("alice", "/orders/1001")
	if r.status != http.StatusOK || r.header.Get("Warning") == "" {
		t.Fatalf("status = %d, Warning = %q", r.status, r.header.Get("Warning"))
	}
	if o := r.order(t); o.Meta.Source != "stale" || o.Order.Name != "#1001" {
		t.Fatalf("meta = %+v", o.Meta)
	}
	// Authorization still applies to cached data.
	if r := s.get("bob", "/orders/1001"); r.status != http.StatusNotFound {
		t.Fatalf("stale cross-customer access: %d", r.status)
	}
}

func TestInboundRateLimiting(t *testing.T) {
	s := newStack(t, func(_ *orders.Config, a *api.Config) { a.RateLimitRPS, a.RateLimitBurst = 1, 3 })
	codes := map[int]int{}
	for range 5 {
		codes[s.get("alice", "/orders/1001").status]++
	}
	if codes[200] != 3 || codes[429] != 2 {
		t.Fatalf("status counts = %v, want 3×200 and 2×429", codes)
	}
	// Limits are per principal.
	if r := s.get("bob", "/orders/1002"); r.status != 200 {
		t.Fatalf("bob throttled by alice's usage: %d", r.status)
	}
}

func TestUpstreamCredentialFailureIsNotRetried(t *testing.T) {
	s := newStack(t, func(o *orders.Config, _ *api.Config) { o.StaleTTL = 0 })
	s.shopify.SetFaults(shopifytest.Faults{FailFirst: 100, FailStatus: http.StatusUnauthorized})
	r := s.get("alice", "/orders/1001")
	if r.status != http.StatusBadGateway {
		t.Fatalf("status = %d", r.status)
	}
	if s.shopify.Requests() != 1 {
		t.Fatalf("upstream requests = %d, want 1", s.shopify.Requests())
	}
}

func (s *stack) mcp(t *testing.T, principal, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+s.keys[principal])
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func toolResult(t *testing.T, out map[string]any) (bool, map[string]any) {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	sc, _ := res["structuredContent"].(map[string]any)
	return res["isError"] == true, sc
}

// The AI assistant reaches OrderGuard over MCP; search and authorization must
// hold end to end.
func TestMCPSearchEndToEnd(t *testing.T) {
	s := newStack(t)
	init := s.mcp(t, "bob", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if init["result"] == nil {
		t.Fatalf("initialize: %v", init)
	}

	search := func(who, query string) []string {
		isErr, sc := toolResult(t, s.mcp(t, who, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_orders","arguments":{"query":`+strconv.Quote(query)+`}}}`))
		if isErr {
			t.Fatalf("%s search %q failed: %v", who, query, sc)
		}
		var ids []string
		for _, r := range sc["results"].([]any) {
			ids = append(ids, r.(map[string]any)["order"].(map[string]any)["id"].(string))
		}
		return ids
	}
	if got := search("bob", "water bottle"); strings.Join(got, ",") != "1002" {
		t.Fatalf("bob water bottle = %v", got)
	}
	if got := search("alice", "water bottle"); len(got) != 0 {
		t.Fatalf("alice must not find bob's order: %v", got)
	}
	if got := search("alice", "jacket|shoe"); strings.Join(got, ",") != "1003,1001" {
		t.Fatalf("alice jacket|shoe = %v", got)
	}
	if got := search("support-bot", "^#100[24]$"); strings.Join(got, ",") != "1004,1002" {
		t.Fatalf("support number regex = %v", got)
	}

	isErr, sc := toolResult(t, s.mcp(t, "bob", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_order","arguments":{"order_id":"#1001"}}}`))
	if !isErr || sc["error"].(map[string]any)["code"] != "order_not_found" {
		t.Fatalf("bob get_order 1001 = %v", sc)
	}
	isErr, sc = toolResult(t, s.mcp(t, "bob", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_order_tracking","arguments":{"order_id":"1002"}}}`))
	if isErr || !strings.Contains(fmt.Sprint(sc), "EX987654321US") {
		t.Fatalf("bob tracking 1002 = %v", sc)
	}

	// Outage: the search list is served from the last good copy, labelled stale.
	s.shopify.SetFaults(shopifytest.Faults{FailRate: 1})
	_, sc = toolResult(t, s.mcp(t, "bob", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"search_orders","arguments":{"query":"bottle"}}}`))
	if sc["meta"].(map[string]any)["source"] != "stale" {
		t.Fatalf("outage search = %v", sc)
	}
}
