// Package demo runs OrderGuard as a self-contained interactive demonstration:
// an in-process fake Shopify server with switchable failure scenarios,
// throwaway users with freshly generated API keys, a single-page UI, and an
// and the AI support assistant (internal/assistant), which reaches OrderGuard
// over MCP with the key of the user the visitor picked.
//
// Demo mode serves only synthetic fixture data and must never be pointed at
// a real store.
package demo

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orderguard/internal/assistant"
	"orderguard/internal/auth"
	"orderguard/internal/observability"
	"orderguard/internal/orders"
	"orderguard/internal/provider"
	"orderguard/internal/provider/fixtures"
	"orderguard/internal/provider/shopify"
	"orderguard/internal/provider/shopify/shopifytest"
	"orderguard/internal/resilience"
)

//go:embed web
var webFS embed.FS

// Config configures demo mode.
type Config struct {
	AssistantModel assistant.Model // nil disables the chat panel
	MCPURL         string          // OrderGuard's own MCP endpoint, used by the assistant
	TrustCFHeader  bool            // use CF-Connecting-IP for per-visitor limits (only behind Cloudflare)
	ChatPerMinute  float64         // per-visitor chat rate
	ChatDailyCap   int             // total chat messages per UTC day across all visitors
	ScenarioTTL    time.Duration   // failure scenarios revert to healthy after this long
	HealthyLatency time.Duration   // simulated Shopify latency when healthy
}

// User is a demo identity with its own API key.
// Only the ID, name and role are sent to the page; keys and customer IDs stay
// on the server (the chat proxy adds the key).
type User struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	CustomerID string `json:"-"`
	APIKey     string `json:"-"`
}

// Demo holds demo-mode state.
type Demo struct {
	cfg   Config
	log   *slog.Logger
	fake  *shopifytest.Server
	users []User
	svc   *orders.Service

	upstreamURL   string
	upstreamToken string
	listener      net.Listener
	srv           *http.Server

	mu       sync.Mutex
	scenario string
	expires  time.Time
	timer    *time.Timer

	chatLimiter *resilience.RateLimiter
	chatDay     string
	chatCount   int
	assistant   *assistant.Assistant
	chats       atomic.Int64
}

// New starts the in-process fake Shopify on a loopback port and generates demo users.
func New(cfg Config, log *slog.Logger) (*Demo, error) {
	token, err := auth.GenerateKey() // random upstream token; never leaves the process
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	d := &Demo{
		cfg:           cfg,
		log:           log.With("component", "demo"),
		fake:          shopifytest.NewServer(token, fixtures.Orders()),
		upstreamToken: token,
		listener:      ln,
		upstreamURL:   "http://" + ln.Addr().String() + "/admin/api/2025-07/graphql.json",
		chatLimiter:   resilience.NewRateLimiter(cfg.ChatPerMinute/60, 3),
	}
	if d.cfg.ScenarioTTL <= 0 {
		d.cfg.ScenarioTTL = 90 * time.Second
	}
	d.srv = &http.Server{Handler: d.fake, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = d.srv.Serve(ln) }()
	d.applyScenario("healthy")

	for _, u := range []User{
		{ID: "alice", Name: "Alice", Role: string(auth.RoleCustomer), CustomerID: fixtures.CustomerAlice},
		{ID: "bob", Name: "Bob", Role: string(auth.RoleCustomer), CustomerID: fixtures.CustomerBob},
		{ID: "support", Name: "Support agent", Role: string(auth.RoleSupport)},
	} {
		if u.APIKey, err = auth.GenerateKey(); err != nil {
			return nil, err
		}
		d.users = append(d.users, u)
	}
	if cfg.AssistantModel != nil {
		support, _ := d.user("support")
		d.assistant, err = assistant.New(assistant.Config{
			Model:        cfg.AssistantModel,
			MCPURL:       cfg.MCPURL,
			DiscoveryKey: support.APIKey, // tool discovery only; chats use the visitor's chosen user
			Log:          d.log,
		})
		if err != nil {
			return nil, err
		}
	}
	return d, nil
}

// Provider returns the Shopify client wired to the in-process fake.
func (d *Demo) Provider() (provider.Provider, error) {
	return shopify.New(shopify.Config{AccessToken: d.upstreamToken, Endpoint: d.upstreamURL, AllowInsecureEndpoint: true})
}

// KeyEntries returns the authenticator entries for the demo users.
func (d *Demo) KeyEntries() []auth.KeyEntry {
	entries := make([]auth.KeyEntry, 0, len(d.users))
	for _, u := range d.users {
		entries = append(entries, auth.KeyEntry{ID: "demo-" + u.ID, KeySHA256: auth.HashKey(u.APIKey), Role: auth.Role(u.Role), CustomerID: u.CustomerID})
	}
	return entries
}

// Attach gives the demo access to the order service (for health and cache purging).
func (d *Demo) Attach(svc *orders.Service) { d.svc = svc }

// Close stops the fake upstream.
func (d *Demo) Close(ctx context.Context) error { return d.srv.Shutdown(ctx) }

type scenarioDef struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	faults      func(healthy time.Duration) shopifytest.Faults
	purge       bool // clear cached copies so the outcome is the same for every visitor
}

var scenarios = map[string]scenarioDef{
	"healthy": {"Normal", "The store answers quickly.",
		func(l time.Duration) shopifytest.Faults { return shopifytest.Faults{Latency: l} }, false},
	"slow": {"Slow", "The store takes too long to answer, so requests give up after a few seconds.",
		func(time.Duration) shopifytest.Faults { return shopifytest.Faults{Latency: 4 * time.Second} }, true},
	"flaky": {"Unreliable", "The store fails twice, then works. OrderGuard quietly tries again.",
		func(l time.Duration) shopifytest.Faults {
			return shopifytest.Faults{Latency: l, FailFirst: 2, FailStatus: http.StatusServiceUnavailable}
		}, true},
	"throttled": {"Busy", "The store asks OrderGuard to wait a moment before trying again.",
		func(l time.Duration) shopifytest.Faults {
			return shopifytest.Faults{Latency: l, FailFirst: 1, FailStatus: http.StatusTooManyRequests, RetryAfterSeconds: 1}
		}, true},
	"outage": {"Down", "The store is offline. Recently seen orders are still shown, marked as saved copies.",
		func(l time.Duration) shopifytest.Faults { return shopifytest.Faults{Latency: l, FailRate: 1} }, false},
}

var scenarioOrder = []string{"healthy", "slow", "flaky", "throttled", "outage"}

func (d *Demo) applyScenario(name string) {
	def := scenarios[name]
	d.fake.SetFaults(def.faults(d.cfg.HealthyLatency))

	d.mu.Lock()
	defer d.mu.Unlock()
	d.scenario = name
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if name != "healthy" {
		d.expires = time.Now().Add(d.cfg.ScenarioTTL)
		d.timer = time.AfterFunc(d.cfg.ScenarioTTL, func() { d.applyScenario("healthy") })
	} else {
		d.expires = time.Time{}
	}
}

// Mount registers the demo routes.
func (d *Demo) Mount(route func(pattern string, h http.Handler)) {
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(static)
	page := pageHeaders(files)

	route("/{$}", page)
	route("/demo/", http.StripPrefix("/demo", page))
	route("GET /demo/api/state", http.HandlerFunc(d.handleState))
	route("PUT /demo/api/scenario", http.HandlerFunc(d.handleScenario))
	route("POST /demo/api/chat", http.HandlerFunc(d.handleChat))
}

// pageHeaders replaces the API's strict CSP with one that allows the page's
// own script and stylesheet (still no inline script and no third parties).
func pageHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

type orderSummary struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	OwnerID           string   `json:"owner_id"` // demo user ID; "" for guest checkouts
	Owner             string   `json:"owner"`
	Items             []string `json:"items"`
	Total             string   `json:"total"`
	CreatedAt         string   `json:"created_at"`
	FinancialStatus   string   `json:"financial_status"`
	FulfillmentStatus string   `json:"fulfillment_status"`
}

type stateResponse struct {
	Users     []User         `json:"users"`
	Orders    []orderSummary `json:"orders"`
	Scenarios []scenarioJSON `json:"scenarios"`
	Scenario  struct {
		Name      string `json:"name"`
		ExpiresIn int    `json:"expires_in_seconds"`
	} `json:"scenario"`
	Upstream struct {
		Requests     int64  `json:"shopify_requests"`
		BreakerState string `json:"circuit_breaker"`
		CachedOrders int    `json:"cached_orders"`
	} `json:"upstream"`
	Assistant struct {
		Enabled bool `json:"enabled"`
	} `json:"assistant"`
}

type scenarioJSON struct {
	Name string `json:"name"`
	scenarioDef
}

func (d *Demo) handleState(w http.ResponseWriter, _ *http.Request) {
	var s stateResponse
	s.Users = d.users
	owners := map[string]User{}
	for _, u := range d.users {
		if u.CustomerID != "" {
			owners[u.CustomerID] = u
		}
	}
	for _, o := range fixtures.Orders() {
		owner := owners[o.CustomerID]
		if o.CustomerID == "" {
			owner.Name = "Guest checkout"
		}
		items := make([]string, 0, len(o.LineItems))
		for _, li := range o.LineItems {
			items = append(items, li.Title)
		}
		s.Orders = append(s.Orders, orderSummary{
			ID: o.ID, Name: o.Name, OwnerID: owner.ID, Owner: owner.Name, Items: items,
			Total: o.Total.Amount + " " + o.Total.Currency, CreatedAt: o.CreatedAt.Format(time.DateOnly),
			FinancialStatus: string(o.FinancialStatus), FulfillmentStatus: string(o.FulfillmentStatus),
		})
	}
	for _, name := range scenarioOrder {
		s.Scenarios = append(s.Scenarios, scenarioJSON{Name: name, scenarioDef: scenarios[name]})
	}
	d.mu.Lock()
	s.Scenario.Name = d.scenario
	if !d.expires.IsZero() {
		s.Scenario.ExpiresIn = max(int(time.Until(d.expires).Seconds()), 0)
	}
	d.mu.Unlock()
	s.Upstream.Requests = d.fake.Requests()
	if d.svc != nil {
		h := d.svc.Health()
		s.Upstream.BreakerState = h.BreakerState.String()
		s.Upstream.CachedOrders = h.StaleCacheItems
	}
	s.Assistant.Enabled = d.assistant != nil
	writeJSON(w, http.StatusOK, s)
}

func (d *Demo) handleScenario(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		PurgeCache *bool  `json:"purge_cache"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "body must be JSON")
		return
	}
	def, ok := scenarios[in.Name]
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "unknown_scenario", "unknown scenario")
		return
	}
	purge := def.purge
	if in.PurgeCache != nil {
		purge = *in.PurgeCache
	}
	if purge && d.svc != nil {
		d.svc.PurgeCache()
	}
	d.applyScenario(in.Name)
	d.log.InfoContext(r.Context(), "demo scenario changed", "scenario", in.Name, "purged_cache", purge)
	d.handleState(w, r)
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func (d *Demo) handleChat(w http.ResponseWriter, r *http.Request) {
	if d.assistant == nil {
		writeErr(w, r, http.StatusServiceUnavailable, "assistant_disabled", "the AI assistant is not configured on this server")
		return
	}
	// The agent may take several model and API round trips.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(100 * time.Second))

	var in struct {
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		Reset     bool   `json:"reset"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "body must be JSON")
		return
	}
	if !sessionPattern.MatchString(in.SessionID) || strings.TrimSpace(in.Message) == "" || len(in.Message) > 1000 {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "session_id and a message of at most 1000 characters are required")
		return
	}
	apiKey := "" // anonymous visitors chat without a key
	if in.UserID != "anonymous" {
		u, ok := d.user(in.UserID)
		if !ok {
			writeErr(w, r, http.StatusBadRequest, "bad_request", "unknown user")
			return
		}
		apiKey = u.APIKey
	}

	if ok, retry := d.chatLimiter.Allow(d.clientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeErr(w, r, http.StatusTooManyRequests, "rate_limited", "you are sending messages too quickly; wait a moment")
		return
	}
	if !d.takeDailyChat() {
		writeErr(w, r, http.StatusTooManyRequests, "daily_limit", "the demo assistant has reached today's message limit; try again tomorrow")
		return
	}

	reply, err := d.assistant.Chat(r.Context(), in.SessionID, in.UserID, apiKey, in.Message, in.Reset)
	switch {
	case err == nil:
		d.chats.Add(1)
		writeJSON(w, http.StatusOK, reply)
	case errors.Is(r.Context().Err(), context.Canceled):
		// the visitor left
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, r, http.StatusGatewayTimeout, "assistant_timeout", "the assistant took too long; please try again")
	default:
		d.log.WarnContext(r.Context(), "assistant turn failed", "error", err)
		writeErr(w, r, http.StatusBadGateway, "assistant_unavailable", "the AI assistant is unavailable")
	}
}

func (d *Demo) user(id string) (User, bool) {
	for _, u := range d.users {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}

func (d *Demo) clientIP(r *http.Request) string {
	if d.cfg.TrustCFHeader {
		if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (d *Demo) takeDailyChat() bool {
	if d.cfg.ChatDailyCap <= 0 {
		return true
	}
	day := time.Now().UTC().Format(time.DateOnly)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.chatDay != day {
		d.chatDay, d.chatCount = day, 0
	}
	if d.chatCount >= d.cfg.ChatDailyCap {
		return false
	}
	d.chatCount++
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": msg, "request_id": observability.RequestID(r.Context()),
	}})
}
