package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orderguard/internal/auth"
	"orderguard/internal/observability"
	"orderguard/internal/orders"
	"orderguard/internal/provider"
	"orderguard/internal/provider/fixtures"
	"orderguard/internal/resilience"
)

// stubService returns canned results so handler behaviour is tested in isolation.
type stubService struct {
	res   *orders.Result
	err   error
	gotID string
	gotP  auth.Principal
	state resilience.State
	panic bool

	gotQuery  string
	gotLimit  int
	searchErr error
}

func (s *stubService) GetOrder(_ context.Context, p auth.Principal, id string) (*orders.Result, error) {
	if s.panic {
		panic("boom")
	}
	s.gotID, s.gotP = id, p
	return s.res, s.err
}

func (s *stubService) SearchOrders(_ context.Context, p auth.Principal, query string, limit int) (*orders.SearchResult, error) {
	s.gotP, s.gotQuery, s.gotLimit = p, query, limit
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	o := *s.res.Order
	return &orders.SearchResult{Matches: []orders.Match{{Order: &o, MatchedOn: []string{"items"}}}, Total: 1,
		Source: s.res.Source, FetchedAt: s.res.FetchedAt}, nil
}

func (s *stubService) Health() orders.Health {
	return orders.Health{Provider: "stub", BreakerState: s.state}
}

type harness struct {
	h       http.Handler
	svc     *stubService
	key     string
	metrics *observability.Metrics
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	key, _ := auth.GenerateKey()
	a, err := auth.NewAuthenticator([]auth.KeyEntry{{ID: "alice", KeySHA256: auth.HashKey(key), Role: auth.RoleCustomer, CustomerID: "7001"}})
	if err != nil {
		t.Fatal(err)
	}
	o := fixtures.Orders()[0]
	svc := &stubService{res: &orders.Result{Order: &o, Source: orders.SourceLive, FetchedAt: time.Unix(1700000000, 0)}}
	m := observability.NewMetrics()
	return &harness{h: NewHandler(cfg, svc, a, slog.New(slog.DiscardHandler), m), svc: svc, key: key, metrics: m}
}

func (h *harness) do(method, path, key string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) ErrorDetail {
	t.Helper()
	var body ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %q", w.Body.String())
	}
	if body.Error.Code == "" || body.Error.RequestID == "" {
		t.Fatalf("error body missing fields: %+v", body)
	}
	return body.Error
}

func TestGetOrderSuccess(t *testing.T) {
	h := newHarness(t, Config{})
	w := h.do("GET", "/orders/1001", h.key)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var resp OrderResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Order.ID != "1001" || resp.Order.EmailMasked != "a***@example.com" || resp.Order.Total.Amount != "89.97" {
		t.Fatalf("order = %+v", resp.Order)
	}
	if resp.Meta.Source != "live" || resp.Meta.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatalf("meta = %+v", resp.Meta)
	}
	if strings.Contains(w.Body.String(), "alice@example.com") {
		t.Fatal("response leaks the full email address")
	}
	if h.svc.gotID != "1001" || h.svc.gotP.ID != "alice" {
		t.Fatalf("service got id=%q principal=%+v", h.svc.gotID, h.svc.gotP)
	}
	for k, v := range map[string]string{
		"Content-Type":           "application/json; charset=utf-8",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestGetFulfillment(t *testing.T) {
	h := newHarness(t, Config{})
	w := h.do("GET", "/orders/1001/fulfillment", h.key)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp FulfillmentResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.FulfillmentStatus != "partially_fulfilled" || len(resp.Fulfillments) != 1 ||
		resp.Fulfillments[0].Tracking[0].Number != "EX123456789US" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestAuthentication(t *testing.T) {
	h := newHarness(t, Config{})
	for name, key := range map[string]string{"missing": "", "wrong": "og_this-key-is-not-configured-anywhere"} {
		w := h.do("GET", "/orders/1001", key)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d", name, w.Code)
		}
		if decodeError(t, w).Code != "unauthorized" || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%s: bad 401 response", name)
		}
	}
	if h.metrics.AuthFailures.Value("missing") != 1 || h.metrics.AuthFailures.Value("invalid") != 1 {
		t.Fatal("auth failure metrics not recorded")
	}
	if h.svc.gotID != "" {
		t.Fatal("unauthenticated request reached the service")
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{&orders.Error{Kind: orders.KindInvalidID}, 400, "invalid_order_id", ""},
		{&orders.Error{Kind: orders.KindNotFound}, 404, "order_not_found", ""},
		{&orders.Error{Kind: orders.KindUnavailable, RetryAfter: 1500 * time.Millisecond}, 503, "upstream_unavailable", "2"},
		{&orders.Error{Kind: orders.KindTimeout}, 504, "upstream_timeout", ""},
		{&orders.Error{Kind: orders.KindUpstream, Err: errors.New("secret internal detail")}, 502, "upstream_error", ""},
		{errors.New("unexpected"), 500, "internal_error", ""},
	}
	for _, tc := range cases {
		h := newHarness(t, Config{})
		h.svc.res, h.svc.err = nil, tc.err
		w := h.do("GET", "/orders/1001", h.key)
		if w.Code != tc.status {
			t.Fatalf("%v: status = %d, want %d", tc.err, w.Code, tc.status)
		}
		if got := decodeError(t, w); got.Code != tc.code {
			t.Fatalf("%v: code = %q, want %q", tc.err, got.Code, tc.code)
		}
		if got := w.Header().Get("Retry-After"); got != tc.retryAfter {
			t.Fatalf("%v: Retry-After = %q, want %q", tc.err, got, tc.retryAfter)
		}
		if strings.Contains(w.Body.String(), "secret internal detail") {
			t.Fatal("internal error detail leaked to client")
		}
	}
}

func TestStaleResponseIsLabelled(t *testing.T) {
	h := newHarness(t, Config{})
	h.svc.res.Source = orders.SourceStale
	w := h.do("GET", "/orders/1001", h.key)
	if w.Code != 200 || w.Header().Get("Warning") == "" {
		t.Fatalf("status = %d, Warning = %q", w.Code, w.Header().Get("Warning"))
	}
	var resp OrderResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Meta.Source != "stale" {
		t.Fatalf("meta.source = %q", resp.Meta.Source)
	}
}

func TestRequestIDPropagation(t *testing.T) {
	h := newHarness(t, Config{})
	w := h.do("GET", "/health", "", "X-Request-ID", "client-abc.123")
	if got := w.Header().Get("X-Request-ID"); got != "client-abc.123" {
		t.Fatalf("X-Request-ID = %q", got)
	}
	w = h.do("GET", "/health", "", "X-Request-ID", "bad id\r\ninjected: 1")
	if got := w.Header().Get("X-Request-ID"); len(got) != 32 {
		t.Fatalf("unsafe request ID not replaced: %q", got)
	}
}

func TestRateLimiting(t *testing.T) {
	h := newHarness(t, Config{RateLimitRPS: 1, RateLimitBurst: 2})
	for i := range 2 {
		if w := h.do("GET", "/orders/1001", h.key); w.Code != 200 {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
	}
	w := h.do("GET", "/orders/1001", h.key)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q", w.Code, w.Header().Get("Retry-After"))
	}
	if decodeError(t, w).Code != "rate_limited" {
		t.Fatal("wrong error code")
	}
}

func TestHealth(t *testing.T) {
	h := newHarness(t, Config{})
	w := h.do("GET", "/health", "")
	var body healthResponse
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || body.Status != "ok" || body.Checks["circuit_breaker"] != "closed" {
		t.Fatalf("status = %d, body = %+v", w.Code, body)
	}
	h.svc.state = resilience.StateOpen
	w = h.do("GET", "/health", "")
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || body.Status != "degraded" {
		t.Fatalf("degraded: status = %d, body = %+v", w.Code, body)
	}
}

func TestRoutingErrors(t *testing.T) {
	h := newHarness(t, Config{})
	if w := h.do("GET", "/nope", ""); w.Code != 404 || decodeError(t, w).Code != "not_found" {
		t.Fatalf("unknown path: %d", w.Code)
	}
	w := h.do("POST", "/orders/1001", h.key)
	if w.Code != 405 || w.Header().Get("Allow") != "GET" || decodeError(t, w).Code != "method_not_allowed" {
		t.Fatalf("POST: %d", w.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t, Config{})
	h.do("GET", "/orders/1001", h.key)
	w := h.do("GET", "/metrics", "")
	body := w.Body.String()
	for _, want := range []string{
		`orderguard_http_requests_total{method="GET",route="/orders/{id}",status="200"} 1`,
		`orderguard_http_request_duration_seconds_count{route="/orders/{id}"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestPanicRecovery(t *testing.T) {
	h := newHarness(t, Config{})
	h.svc.panic = true
	w := h.do("GET", "/orders/1001", h.key)
	if w.Code != 500 || decodeError(t, w).Code != "internal_error" {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestLoadShedding(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-gate
	})
	h := loadShed(1, observability.NewMetrics())(slow)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		close(done)
	}()
	<-entered
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	close(gate)
	<-done
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"alice@example.com": "a***@example.com",
		"x@y.z":             "x***@y.z",
		"":                  "",
		"no-at-sign":        "",
		"@example.com":      "",
	} {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDTOOmitsUnknownTimestamps(t *testing.T) {
	o := provider.Order{ID: "1", Name: "#1", CreatedAt: time.Unix(1, 0)}
	b, _ := json.Marshal(toOrderDTO(&o))
	if strings.Contains(string(b), "updated_at") || strings.Contains(string(b), "0001-01-01") {
		t.Fatalf("zero timestamps rendered: %s", b)
	}
}

func TestSearchEndpoint(t *testing.T) {
	h := newHarness(t, Config{})
	w := h.do("GET", "/orders?q=water+bottle&limit=5", h.key)
	if w.Code != 200 {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	var resp SearchResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 1 || resp.Results[0].MatchedOn[0] != "items" || resp.TotalMatches != 1 || resp.Meta.Source != "live" {
		t.Fatalf("resp = %+v", resp)
	}
	if h.svc.gotQuery != "water bottle" || h.svc.gotLimit != 5 || h.svc.gotP.ID != "alice" {
		t.Fatalf("service got q=%q limit=%d p=%+v", h.svc.gotQuery, h.svc.gotLimit, h.svc.gotP)
	}
	if w := h.do("GET", "/orders?limit=500", h.key); w.Code != 400 || decodeError(t, w).Code != "invalid_limit" {
		t.Fatalf("bad limit: %d", w.Code)
	}
	h.svc.searchErr = &orders.Error{Kind: orders.KindInvalidQuery}
	if w := h.do("GET", "/orders?q=(", h.key); w.Code != 400 || decodeError(t, w).Code != "invalid_query" {
		t.Fatalf("bad query: %d", w.Code)
	}
	if w := h.do("GET", "/orders", ""); w.Code != 401 {
		t.Fatalf("unauthenticated search: %d", w.Code)
	}
}

func (h *harness) mcp(body string, hdr ...string) (int, map[string]any) {
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+h.key)
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestMCPHandshakeAndTools(t *testing.T) {
	h := newHarness(t, Config{})
	code, out := h.mcp(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	res, _ := out["result"].(map[string]any)
	if code != 200 || res["protocolVersion"] != "2025-03-26" || res["capabilities"] == nil {
		t.Fatalf("initialize = %d %v", code, out)
	}
	if code, _ := h.mcp(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification status = %d", code)
	}
	_, out = h.mcp(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := out["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "search_orders,get_order,get_order_tracking" {
		t.Fatalf("tools = %v", names)
	}

	_, out = h.mcp(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_orders","arguments":{"query":"bottle"}}}`)
	res = out["result"].(map[string]any)
	if res["isError"] != false || h.svc.gotQuery != "bottle" {
		t.Fatalf("search result = %v", out)
	}
	sc := res["structuredContent"].(map[string]any)
	if len(sc["results"].([]any)) != 1 {
		t.Fatalf("structured = %v", sc)
	}

	_, out = h.mcp(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_order","arguments":{"order_id":"#1001"}}}`)
	if out["result"].(map[string]any)["isError"] != false || h.svc.gotID != "1001" {
		t.Fatalf("get_order = %v (id %q)", out, h.svc.gotID)
	}

	h.svc.err = &orders.Error{Kind: orders.KindNotFound}
	_, out = h.mcp(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_order_tracking","arguments":{"order_id":"1002"}}}`)
	res = out["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "order_not_found") {
		t.Fatalf("not found = %v", out)
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	h := newHarness(t, Config{})
	cases := map[string]float64{
		`not json`: -32700,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`:                                                         -32600,
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`:                                               -32601,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_order","arguments":{}}}`:   -32602,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_order","arguments":{}}}`:      -32602,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_order","arguments":{"x":1}}}`: -32602,
	}
	for body, want := range cases {
		_, out := h.mcp(body)
		e, _ := out["error"].(map[string]any)
		if e == nil || e["code"] != want {
			t.Errorf("%s: got %v, want error %v", body, out, want)
		}
	}
	if code, out := h.mcp(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 200 || out["result"] == nil {
		t.Fatalf("ping = %d %v", code, out)
	}
	if code, _ := h.mcp(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://evil.example"); code != 403 {
		t.Fatalf("cross-origin status = %d", code)
	}
	w := h.do("GET", "/mcp", h.key)
	if w.Code != 405 {
		t.Fatalf("GET /mcp = %d", w.Code)
	}
	if w := h.do("POST", "/mcp", ""); w.Code != 401 {
		t.Fatalf("unauthenticated MCP = %d", w.Code)
	}
}
