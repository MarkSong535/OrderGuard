package demo

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"orderguard/internal/api"
	"orderguard/internal/assistant"
	"orderguard/internal/auth"
	"orderguard/internal/observability"
	"orderguard/internal/orders"
	"orderguard/internal/resilience"
)

type harness struct {
	t   *testing.T
	d   *Demo
	srv *httptest.Server
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	// The assistant needs this server's own MCP URL, so start it before the handler exists.
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	cfg.MCPURL = srv.URL + "/mcp"
	d, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })

	authn, err := auth.NewAuthenticator(d.KeyEntries())
	if err != nil {
		t.Fatal(err)
	}
	prov, err := d.Provider()
	if err != nil {
		t.Fatal(err)
	}
	m := observability.NewMetrics()
	svc := orders.NewService(prov, orders.Config{
		AttemptTimeout: 300 * time.Millisecond, RequestTimeout: time.Second,
		Retry:            resilience.RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 20 * time.Millisecond},
		BreakerThreshold: 6, BreakerCooldown: time.Minute, MaxConcurrent: 16, BulkheadWait: 100 * time.Millisecond,
		StaleTTL: time.Minute, StaleCacheSize: 10,
	}, log, m)
	d.Attach(svc)
	handler = api.NewHandler(api.Config{Mount: d.Mount, RateLimitRPS: 100, RateLimitBurst: 100}, svc, authn, log, m)
	return &harness{t: t, d: d, srv: srv}
}

func (h *harness) do(method, path, key, body string) (*http.Response, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (h *harness) key(id string) string {
	u, ok := h.d.user(id)
	if !ok {
		h.t.Fatalf("no demo user %q", id)
	}
	return u.APIKey
}

func (h *harness) state() stateResponse {
	h.t.Helper()
	_, b := h.do("GET", "/demo/api/state", "", "")
	var s stateResponse
	if err := json.Unmarshal(b, &s); err != nil {
		h.t.Fatalf("state: %v: %s", err, b)
	}
	return s
}

func TestPageIsServedWithItsOwnCSP(t *testing.T) {
	h := newHarness(t, Config{})
	for _, path := range []string{"/", "/demo/"} {
		resp, body := h.do("GET", path, "", "")
		if resp.StatusCode != 200 || !strings.Contains(string(body), "OrderGuard") {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
			t.Fatalf("%s: CSP = %q", path, csp)
		}
	}
	_, page := h.do("GET", "/", "", "")
	assets := regexp.MustCompile(`/demo/assets/[A-Za-z0-9._-]+\.(js|css)`).FindAllString(string(page), -1)
	if len(assets) < 2 {
		t.Fatalf("page does not reference built assets: %s", page)
	}
	for _, asset := range assets {
		if resp, _ := h.do("GET", asset, "", ""); resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", asset, resp.StatusCode)
		}
	}
	if strings.Contains(string(page), "<script>") || strings.Contains(string(page), "style=") {
		t.Fatal("page must not need inline script or style (CSP)")
	}
	// API routes keep their strict policy.
	resp, _ := h.do("GET", "/health", "", "")
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("API CSP was weakened")
	}
}

func TestStateDescribesUsersOrdersAndScenarios(t *testing.T) {
	h := newHarness(t, Config{})
	s := h.state()
	if len(s.Users) != 3 || len(s.Orders) != 4 || len(s.Scenarios) != len(scenarioOrder) {
		t.Fatalf("state = %+v", s)
	}
	if s.Scenario.Name != "healthy" || s.Upstream.BreakerState != "closed" || s.Assistant.Enabled {
		t.Fatalf("initial state = %+v", s)
	}
	if s.Orders[3].Owner != "Guest checkout" || s.Orders[3].OwnerID != "" || s.Orders[1].OwnerID != "bob" {
		t.Fatalf("owners = %+v", s.Orders)
	}
	_, raw := h.do("GET", "/demo/api/state", "", "")
	for _, secret := range []string{h.key("alice"), h.key("bob"), h.key("support"), "7001", "api_key", "customer_id"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("state leaks %q", secret)
		}
	}
}

func TestGeneratedKeysWorkAgainstTheRealAPI(t *testing.T) {
	h := newHarness(t, Config{})
	if resp, _ := h.do("GET", "/orders/1001", h.key("alice"), ""); resp.StatusCode != 200 {
		t.Fatalf("alice: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/orders/1001", h.key("bob"), ""); resp.StatusCode != 404 {
		t.Fatalf("bob: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/orders/1004", h.key("support"), ""); resp.StatusCode != 200 {
		t.Fatalf("support: %d", resp.StatusCode)
	}
	// Keys differ every time the server starts.
	other := newHarness(t, Config{})
	if other.key("alice") == h.key("alice") {
		t.Fatal("demo keys are not random")
	}
}

func TestScenarios(t *testing.T) {
	h := newHarness(t, Config{})
	alice := h.key("alice")

	if resp, _ := h.do("PUT", "/demo/api/scenario", "", `{"name":"nope"}`); resp.StatusCode != 400 {
		t.Fatalf("unknown scenario: %d", resp.StatusCode)
	}

	// Slow purges the cache, so the timeout is visible to everyone. (It runs
	// first: the outage below takes the breaker to its threshold.)
	h.do("GET", "/orders/1003", alice, "")
	h.do("PUT", "/demo/api/scenario", "", `{"name":"slow"}`)
	if resp, _ := h.do("GET", "/orders/1003", alice, ""); resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("slow: %d", resp.StatusCode)
	}

	// Flaky: two failures are retried.
	h.do("PUT", "/demo/api/scenario", "", `{"name":"flaky"}`)
	before := h.d.fake.Requests()
	if resp, _ := h.do("GET", "/orders/1003", alice, ""); resp.StatusCode != 200 {
		t.Fatalf("flaky: %d", resp.StatusCode)
	}
	if got := h.d.fake.Requests() - before; got != 3 {
		t.Fatalf("flaky upstream calls = %d, want 3", got)
	}

	// Outage: the cached order is served stale.
	h.do("PUT", "/demo/api/scenario", "", `{"name":"outage"}`)
	resp, body := h.do("GET", "/orders/1003", alice, "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"source":"stale"`) {
		t.Fatalf("outage: %d %s", resp.StatusCode, body)
	}
	if s := h.state(); s.Scenario.Name != "outage" || s.Scenario.ExpiresIn < 80 {
		t.Fatalf("scenario state = %+v", s.Scenario)
	}
}

func TestScenarioResetsToHealthy(t *testing.T) {
	h := newHarness(t, Config{ScenarioTTL: 100 * time.Millisecond})
	h.do("PUT", "/demo/api/scenario", "", `{"name":"outage"}`)
	if s := h.state(); s.Scenario.Name != "outage" {
		t.Fatalf("scenario = %+v", s.Scenario)
	}
	time.Sleep(200 * time.Millisecond)
	if s := h.state(); s.Scenario.Name != "healthy" || s.Scenario.ExpiresIn != 0 {
		t.Fatalf("scenario did not reset: %+v", s.Scenario)
	}

}

// fakeModel answers every turn by searching once, then replying with what it found.
type fakeModel struct {
	mu    sync.Mutex
	turns int
	seen  []string
}

func (m *fakeModel) Complete(_ context.Context, msgs []assistant.Message, _ []assistant.Tool, _ string) (assistant.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := msgs[len(msgs)-1]
	tc := assistant.ToolCall{ID: "c", Type: "function"}
	if last.Role == "tool" {
		m.seen = append(m.seen, *last.Content)
		tc.Function.Name, tc.Function.Arguments = "final_answer_tool", `{"answer":"done"}`
	} else {
		m.turns++
		tc.Function.Name, tc.Function.Arguments = "search_orders", `{"query":"bottle"}`
	}
	return assistant.Message{Role: "assistant", ToolCalls: []assistant.ToolCall{tc}}, nil
}

func TestChatRunsTheAssistantAsTheChosenUser(t *testing.T) {
	model := &fakeModel{}
	h := newHarness(t, Config{AssistantModel: model, ChatDailyCap: 4})
	if !h.state().Assistant.Enabled {
		t.Fatal("assistant should be enabled")
	}

	chat := func(user string) (int, map[string]any) {
		resp, body := h.do("POST", "/demo/api/chat", "", `{"user_id":"`+user+`","session_id":"session-1234","message":"water bottle?"}`)
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return resp.StatusCode, out
	}
	code, out := chat("bob")
	calls, _ := out["tool_calls"].([]any)
	if code != 200 || out["answer"] != "done" || len(calls) != 1 || calls[0].(map[string]any)["results"] != float64(1) {
		t.Fatalf("bob: %d %v", code, out)
	}
	if !strings.Contains(model.seen[0], "Insulated Water Bottle") {
		t.Fatal("bob's search should find his water bottle via MCP")
	}
	chat("alice")
	if strings.Contains(model.seen[1], "Insulated Water Bottle") {
		t.Fatal("alice must not see bob's order")
	}
	_, out = chat("anonymous")
	if c := out["tool_calls"].([]any)[0].(map[string]any); c["error"] != "unauthorized" {
		t.Fatalf("guest call = %v", c)
	}

	for _, bad := range []string{
		`{"user_id":"mallory","session_id":"session-1234","message":"hi"}`,
		`{"user_id":"alice","session_id":"x","message":"hi"}`,
		`{"user_id":"alice","session_id":"session-1234","message":""}`,
		`not json`,
	} {
		if resp, _ := h.do("POST", "/demo/api/chat", "", bad); resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", bad, resp.StatusCode)
		}
	}

	chat("bob") // 4th message: the daily cap
	if code, out := chat("bob"); code != 429 || out["error"].(map[string]any)["code"] != "daily_limit" {
		t.Fatalf("daily cap: %d %v", code, out)
	}
}

func TestChatPerVisitorRateLimit(t *testing.T) {
	h := newHarness(t, Config{AssistantModel: &fakeModel{}, ChatPerMinute: 1, TrustCFHeader: true})

	send := func(ip string) int {
		req, _ := http.NewRequest("POST", h.srv.URL+"/demo/api/chat", strings.NewReader(`{"user_id":"alice","session_id":"session-1234","message":"hi"}`))
		req.Header.Set("CF-Connecting-IP", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := range 3 { // burst of 3
		if s := send("203.0.113.7"); s != 200 {
			t.Fatalf("message %d: %d", i, s)
		}
	}
	if s := send("203.0.113.7"); s != 429 {
		t.Fatalf("4th message: %d, want 429", s)
	}
	if s := send("198.51.100.9"); s != 200 {
		t.Fatalf("other visitor: %d", s)
	}
}

func TestChatDisabled(t *testing.T) {
	h := newHarness(t, Config{})
	resp, body := h.do("POST", "/demo/api/chat", "", `{"user_id":"alice","session_id":"session-1234","message":"hi"}`)
	if resp.StatusCode != 503 || !strings.Contains(string(body), "assistant_disabled") {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
}
