package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orderguard/internal/api"
	"orderguard/internal/auth"
	"orderguard/internal/observability"
	"orderguard/internal/orders"
	"orderguard/internal/provider/fixtures"
	"orderguard/internal/provider/shopify"
	"orderguard/internal/provider/shopify/shopifytest"
	"orderguard/internal/resilience"
)

// scripted plays back one model reply per request and records every request,
// like canvas_ai's FakeCompletions.
type scripted struct {
	mu      sync.Mutex
	replies []Message
	calls   []request
	fail    error
}

type request struct {
	Messages   []Message
	Tools      []Tool
	ToolChoice string
}

func (m *scripted) Complete(_ context.Context, msgs []Message, tools []Tool, choice string) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, request{append([]Message(nil), msgs...), tools, choice})
	if m.fail != nil {
		return Message{}, m.fail
	}
	if len(m.replies) == 0 {
		return Message{}, errors.New("script exhausted")
	}
	r := m.replies[0]
	m.replies = m.replies[1:]
	return r, nil
}

func (m *scripted) text(i int) string {
	b, _ := json.Marshal(m.calls[i].Messages)
	return string(b)
}

func call(id, name, args string) Message {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name, tc.Function.Arguments = name, args
	return Message{Role: "assistant", ToolCalls: []ToolCall{tc}}
}

func answer(id, text string) Message {
	b, _ := json.Marshal(map[string]string{"answer": text})
	return call(id, finalAnswerTool, string(b))
}

type stack struct {
	a     *Assistant
	model *scripted
	shop  *shopifytest.Server
	keys  map[string]string
}

// newStack wires the assistant to OrderGuard's real MCP endpoint over HTTP.
func newStack(t *testing.T, replies ...Message) *stack {
	t.Helper()
	fake := shopifytest.NewServer("tok", fixtures.Orders())
	up := httptest.NewServer(fake)
	t.Cleanup(up.Close)
	client, err := shopify.New(shopify.Config{AccessToken: "tok", Endpoint: up.URL + "/graphql.json", AllowInsecureEndpoint: true})
	if err != nil {
		t.Fatal(err)
	}

	keys := map[string]string{}
	var entries []auth.KeyEntry
	for _, e := range []auth.KeyEntry{
		{ID: "alice", Role: auth.RoleCustomer, CustomerID: fixtures.CustomerAlice},
		{ID: "bob", Role: auth.RoleCustomer, CustomerID: fixtures.CustomerBob},
		{ID: "support", Role: auth.RoleSupport},
	} {
		k, _ := auth.GenerateKey()
		keys[e.ID], e.KeySHA256 = k, auth.HashKey(k)
		entries = append(entries, e)
	}
	authn, err := auth.NewAuthenticator(entries)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	m := observability.NewMetrics()
	svc := orders.NewService(client, orders.Config{
		AttemptTimeout: 300 * time.Millisecond, RequestTimeout: time.Second,
		Retry:            resilience.RetryPolicy{MaxAttempts: 2, BaseDelay: 5 * time.Millisecond},
		BreakerThreshold: 50, BreakerCooldown: time.Minute, MaxConcurrent: 8, BulkheadWait: 100 * time.Millisecond,
		StaleTTL: time.Minute, StaleCacheSize: 50,
	}, log, m)
	og := httptest.NewServer(api.NewHandler(api.Config{}, svc, authn, log, m))
	t.Cleanup(og.Close)

	model := &scripted{replies: replies}
	a, err := New(Config{Model: model, MCPURL: og.URL + "/mcp", DiscoveryKey: keys["support"]})
	if err != nil {
		t.Fatal(err)
	}
	return &stack{a: a, model: model, shop: fake, keys: keys}
}

func (s *stack) chat(t *testing.T, user, message string, reset bool) *Reply {
	t.Helper()
	r, err := s.a.Chat(context.Background(), "session-1", user, s.keys[user], message, reset)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	return r
}

func toolNames(r request) []string {
	var out []string
	for _, t := range r.Tools {
		out = append(out, t.Function.Name)
	}
	return out
}

func TestFindsOrderByItemViaMCPSearch(t *testing.T) {
	s := newStack(t,
		call("c1", "search_orders", `{"query":"bottle","limit":null}`),
		answer("c2", "Your water bottle is in order #1002."))
	r := s.chat(t, "bob", "whats my water bottle order's number", false)

	if r.Answer != "Your water bottle is in order #1002." {
		t.Fatalf("answer = %q", r.Answer)
	}
	if len(r.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", r.ToolCalls)
	}
	c := r.ToolCalls[0]
	if c.Tool != "search_orders" || !c.OK || c.Results == nil || *c.Results != 1 || c.Source != "live" {
		t.Fatalf("call = %+v", c)
	}
	if _, sent := c.Args["limit"]; sent {
		t.Fatal("null optional argument should be dropped")
	}
	first := s.model.calls[0]
	if got := strings.Join(toolNames(first), ","); got != "search_orders,get_order,get_order_tracking,final_answer_tool" {
		t.Fatalf("tools offered = %s", got)
	}
	if first.ToolChoice != "required" || first.Messages[0].Role != "system" || !strings.Contains(first.Messages[0].text(), "search_orders") {
		t.Fatal("first request must carry the system prompt, the tools and tool_choice=required")
	}
	if !strings.Contains(s.model.text(1), "Insulated Water Bottle") {
		t.Fatal("the model should see the real search observation")
	}
}

func TestAuthorizationIsEnforcedByOrderGuardNotThePrompt(t *testing.T) {
	s := newStack(t,
		call("c1", "get_order", `{"order_id":"#1001"}`),
		answer("c2", "I couldn't find order #1001 on your account."))
	r := s.chat(t, "bob", "Show order 1001", false)

	if c := r.ToolCalls[0]; c.OK || c.Error != "order_not_found" {
		t.Fatalf("call = %+v", c)
	}
	obs := s.model.text(1)
	if !strings.Contains(obs, "order_not_found") || strings.Contains(obs, "Trail Running Shoe") {
		t.Fatalf("observation leaked or missing: %s", obs)
	}
}

func TestGuestGetsToolsButIsRefused(t *testing.T) {
	s := newStack(t,
		call("c1", "search_orders", `{"query":""}`),
		answer("c2", "Please sign in to see your orders."))
	r := s.chat(t, "anonymous", "list my orders", false) // no key

	if len(toolNames(s.model.calls[0])) != 4 {
		t.Fatal("guests should get the same tools (discovered with the support key)")
	}
	if c := r.ToolCalls[0]; c.Error != "unauthorized" {
		t.Fatalf("call = %+v", c)
	}
	if !strings.Contains(s.model.text(1), "not signed in") {
		t.Fatal("the model should be told the customer is not signed in")
	}
}

func TestConversationMemoryAndPerUserSessions(t *testing.T) {
	s := newStack(t,
		call("c1", "get_order", `{"order_id":"1001"}`),
		answer("c2", "Order #1001 has running shoes and socks."),
		answer("c3", "It is partially fulfilled."),
		answer("c4", "Which order do you mean?"))
	s.chat(t, "alice", "What's in order 1001?", false)
	if r := s.chat(t, "alice", "And its status?", false); len(r.ToolCalls) != 0 {
		t.Fatalf("follow-up made calls: %+v", r.ToolCalls)
	}
	if !strings.Contains(s.model.text(2), "Trail Running Shoe") {
		t.Fatal("follow-up should see the earlier turn")
	}
	s.chat(t, "bob", "Hi", false)
	if strings.Contains(s.model.text(3), "Trail Running Shoe") {
		t.Fatal("another user's conversation must not share memory")
	}
}

func TestResetStartsANewConversation(t *testing.T) {
	s := newStack(t, answer("c1", "Hello!"), answer("c2", "Hello again!"))
	s.chat(t, "alice", "remember the word pineapple", false)
	s.chat(t, "alice", "hi", true)
	if strings.Contains(s.model.text(1), "pineapple") {
		t.Fatal("reset should forget the earlier turn")
	}
}

func TestRepeatedCallIsAnsweredFromMemory(t *testing.T) {
	s := newStack(t,
		call("c1", "get_order", `{"order_id":"1003"}`),
		call("c2", "get_order", `{"order_id":"1003"}`),
		answer("c3", "Your jacket hasn't shipped yet."))
	before := s.shop.Requests()
	r := s.chat(t, "alice", "Has my jacket shipped?", false)
	if len(r.ToolCalls) != 1 || s.shop.Requests()-before != 1 {
		t.Fatalf("calls = %d, upstream = %d; the repeat should not hit OrderGuard", len(r.ToolCalls), s.shop.Requests()-before)
	}
	if !strings.Contains(s.model.text(2), "You already called this tool") {
		t.Fatal("the repeat should be flagged to the model")
	}
}

func TestStepLimitFallsBackToAnAnswer(t *testing.T) {
	s := newStack(t,
		call("c1", "search_orders", `{"query":"a"}`),
		call("c2", "search_orders", `{"query":"b"}`),
		Message{Role: "assistant", Content: text("I couldn't complete the lookup; please try again.")})
	s.a.cfg.MaxSteps = 2
	r := s.chat(t, "alice", "find something", false)
	last := s.model.calls[2]
	if r.Answer != "I couldn't complete the lookup; please try again." || last.Tools != nil || last.Messages[0].text() != finalPre {
		t.Fatalf("fallback: answer=%q tools=%v", r.Answer, last.Tools)
	}
}

func TestStaleDataIsLabelled(t *testing.T) {
	s := newStack(t,
		call("c1", "get_order_tracking", `{"order_id":"1002"}`),
		answer("c2", "Delivered."),
		call("c3", "get_order_tracking", `{"order_id":"1002"}`),
		answer("c4", "Delivered (as of the last update)."))
	s.chat(t, "bob", "Where is order 1002?", false)
	s.shop.SetFaults(shopifytest.Faults{FailRate: 1})
	r := s.chat(t, "bob", "Check again please", true)
	if c := r.ToolCalls[0]; !c.OK || c.Source != "stale" {
		t.Fatalf("call = %+v", c)
	}
	if obs := s.model.text(3); !strings.Contains(obs, "STALE DATA") || !strings.Contains(obs, " UTC") {
		t.Fatal("the model should be told the data is a saved copy, with a readable time")
	}
	if got := readableTime("2026-10-23T20:16:45.123Z"); got != "on 23 Oct 2026 at 20:16 UTC" {
		t.Fatalf("readableTime = %q", got)
	}
}

func TestBadToolCallsAndFailures(t *testing.T) {
	s := newStack(t,
		call("c1", "get_order", `{not json`),
		call("c2", "delete_order", `{}`),
		answer("c3", "Sorry."))
	r := s.chat(t, "alice", "hi", false)
	obs := s.model.text(2)
	if len(r.ToolCalls) != 0 || !strings.Contains(obs, "not valid JSON") || !strings.Contains(obs, "unknown tool delete_order") {
		t.Fatalf("calls = %+v, observations = %s", r.ToolCalls, obs)
	}

	s.model.fail = errors.New("provider exploded")
	if _, err := s.a.Chat(context.Background(), "session-2", "alice", s.keys["alice"], "hi", false); err == nil {
		t.Fatal("model failure should be returned")
	}
}

func TestUnreachableOrderGuard(t *testing.T) {
	model := &scripted{replies: []Message{call("c1", "get_order", `{"order_id":"1001"}`), answer("c2", "Sorry.")}}
	a, _ := New(Config{Model: model, MCPURL: "http://127.0.0.1:9/mcp", HTTP: &http.Client{Timeout: time.Second}})
	if _, err := a.Chat(context.Background(), "s", "alice", "k", "hi", false); err == nil {
		t.Fatal("tool discovery against a dead server should fail the turn")
	}
	a.tools = []MCPTool{{Name: "get_order", InputSchema: map[string]any{"type": "object"}}} // as if discovered earlier
	r, err := a.Chat(context.Background(), "s", "alice", "k", "hi", false)
	if err != nil || r.ToolCalls[0].Error != "unreachable" || !strings.Contains(model.text(1), "order_system_unreachable") {
		t.Fatalf("reply = %+v, err = %v", r, err)
	}
}

func TestOpenAIClient(t *testing.T) {
	var got chatRequest
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got.Model == "bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: sk-secret","type":"invalid_request_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"final_answer_tool","arguments":"{\"answer\":\"hi\"}"}}]}}]}`))
	}))
	defer srv.Close()

	c := &OpenAI{BaseURL: srv.URL + "/v1/", APIKey: "sk-test", Model: "gpt-6-luna", ReasoningEffort: "none", HTTP: srv.Client()}
	msg, err := c.Complete(context.Background(), []Message{{Role: "user", Content: text("hi")}}, toolDefs(nil), "required")
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := finalAnswer(msg); !ok || a != "hi" {
		t.Fatalf("answer = %q", a)
	}
	if auth != "Bearer sk-test" || got.Model != "gpt-6-luna" || got.ToolChoice != "required" || got.ReasoningEffort != "none" || len(got.Tools) != 1 {
		t.Fatalf("request = %+v (auth %q)", got, auth)
	}

	c.Model = "bad"
	_, err = c.Complete(context.Background(), nil, nil, "")
	if err == nil || strings.Contains(err.Error(), "sk-secret") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v", err)
	}
}

func TestSSEReplies(t *testing.T) {
	if got := string(lastSSEData([]byte("event: message\ndata: {\"a\":1}\n\ndata: {\"b\":2}\n\n"))); got != `{"b":2}` {
		t.Fatalf("lastSSEData = %q", got)
	}
}
