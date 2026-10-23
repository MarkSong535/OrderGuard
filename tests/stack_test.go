// Package tests holds end-to-end tests that run the real HTTP stack
// (API → order service → Shopify client) against a fake Shopify server over
// loopback HTTP.
package tests

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

const shopifyToken = "shpat_integration_test"

type stack struct {
	t       *testing.T
	url     string
	shopify *shopifytest.Server
	svc     *orders.Service
	metrics *observability.Metrics
	keys    map[string]string // principal ID -> plaintext key
	client  *http.Client
}

type stackOption func(*orders.Config, *api.Config)

func defaultOrdersConfig() orders.Config {
	return orders.Config{
		AttemptTimeout:   200 * time.Millisecond,
		RequestTimeout:   time.Second,
		Retry:            resilience.RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond, Jitter: true},
		BreakerThreshold: 5,
		BreakerCooldown:  time.Minute,
		MaxConcurrent:    64,
		BulkheadWait:     500 * time.Millisecond,
		StaleTTL:         time.Minute,
		StaleCacheSize:   100,
	}
}

func newStack(t testing.TB, opts ...stackOption) *stack {
	t.Helper()
	fake := shopifytest.NewServer(shopifyToken, fixtures.Orders())
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)

	client, err := shopify.New(shopify.Config{
		AccessToken:           shopifyToken,
		Endpoint:              upstream.URL + "/admin/api/2025-07/graphql.json",
		AllowInsecureEndpoint: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ocfg, acfg := defaultOrdersConfig(), api.Config{MaxInFlight: 1024}
	for _, o := range opts {
		o(&ocfg, &acfg)
	}

	keys := map[string]string{}
	var entries []auth.KeyEntry
	for _, e := range []auth.KeyEntry{
		{ID: "alice", Role: auth.RoleCustomer, CustomerID: fixtures.CustomerAlice},
		{ID: "bob", Role: auth.RoleCustomer, CustomerID: fixtures.CustomerBob},
		{ID: "support-bot", Role: auth.RoleSupport},
	} {
		k, err := auth.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		keys[e.ID] = k
		e.KeySHA256 = auth.HashKey(k)
		entries = append(entries, e)
	}
	authn, err := auth.NewAuthenticator(entries)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	m := observability.NewMetrics()
	svc := orders.NewService(client, ocfg, log, m)
	srv := httptest.NewServer(api.NewHandler(acfg, svc, authn, log, m))
	t.Cleanup(srv.Close)

	tt, _ := t.(*testing.T)
	return &stack{
		t: tt, url: srv.URL, shopify: fake, svc: svc, metrics: m, keys: keys,
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256}},
	}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (s *stack) get(principal, path string) response {
	req, _ := http.NewRequest(http.MethodGet, s.url+path, nil)
	if k, ok := s.keys[principal]; ok {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if s.t != nil {
			s.t.Fatalf("GET %s: %v", path, err)
		}
		panic(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, b}
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	var e api.ErrorBody
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("not an error body: %s", r.body)
	}
	return e.Error.Code
}

func (r response) order(t *testing.T) api.OrderResponse {
	t.Helper()
	var o api.OrderResponse
	if err := json.Unmarshal(r.body, &o); err != nil {
		t.Fatalf("not an order body: %s", r.body)
	}
	return o
}
