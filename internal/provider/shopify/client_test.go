package shopify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orderguard/internal/provider"
	"orderguard/internal/provider/fixtures"
	"orderguard/internal/provider/shopify"
	"orderguard/internal/provider/shopify/shopifytest"
)

const testToken = "shpat_test_token"

func newClient(t *testing.T, h http.Handler) *shopify.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := shopify.New(shopify.Config{
		AccessToken:           testToken,
		Endpoint:              srv.URL + "/admin/api/2025-07/graphql.json",
		AllowInsecureEndpoint: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wantKind(t *testing.T, err error, want provider.Kind) *provider.Error {
	t.Helper()
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *provider.Error", err, err)
	}
	if pe.Kind != want {
		t.Fatalf("kind = %v, want %v (err: %v)", pe.Kind, want, err)
	}
	return pe
}

func TestGetOrderNormalizesResponse(t *testing.T) {
	c := newClient(t, shopifytest.NewServer(testToken, fixtures.Orders()))
	o, err := c.GetOrder(context.Background(), "1001")
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != "1001" || o.Name != "#1001" || o.CustomerID != fixtures.CustomerAlice {
		t.Fatalf("identity fields wrong: %+v", o)
	}
	if o.FinancialStatus != provider.FinancialPaid || o.FulfillmentStatus != provider.FulfillmentPartiallyFulfilled {
		t.Fatalf("statuses = %q/%q", o.FinancialStatus, o.FulfillmentStatus)
	}
	if o.Total != (provider.Money{Amount: "89.97", Currency: "USD"}) {
		t.Fatalf("total = %+v", o.Total)
	}
	if len(o.LineItems) != 2 || o.LineItems[1].Quantity != 2 {
		t.Fatalf("line items = %+v", o.LineItems)
	}
	if len(o.Fulfillments) != 1 || o.Fulfillments[0].DisplayStatus != "in_transit" ||
		o.Fulfillments[0].Tracking[0].Number != "EX123456789US" {
		t.Fatalf("fulfillments = %+v", o.Fulfillments)
	}
}

func TestGetOrderGuestHasNoCustomer(t *testing.T) {
	c := newClient(t, shopifytest.NewServer(testToken, fixtures.Orders()))
	o, err := c.GetOrder(context.Background(), "1004")
	if err != nil {
		t.Fatal(err)
	}
	if o.CustomerID != "" || o.LineItems[0].SKU != "" {
		t.Fatalf("guest order = %+v", o)
	}
}

func TestGetOrderSendsCredentialsAndQuery(t *testing.T) {
	var got shopify.GraphQLRequest
	var token string
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("X-Shopify-Access-Token")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"data":{"order":null}}`))
	}))
	_, _ = c.GetOrder(context.Background(), "42")
	if token != testToken {
		t.Fatalf("token header = %q", token)
	}
	if got.Variables["id"] != "gid://shopify/Order/42" || !strings.Contains(got.Query, "order(id: $id)") {
		t.Fatalf("request = %+v", got)
	}
}

func TestGetOrderErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		kind    provider.Kind
		retry   time.Duration
	}{
		{"not found", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"order":null}}`))
		}, provider.KindNotFound, 0},
		{"http 429", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
		}, provider.KindRateLimited, 2 * time.Second},
		{"graphql throttled", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"errors":[{"message":"Throttled","extensions":{"code":"THROTTLED"}}],
				"extensions":{"cost":{"requestedQueryCost":250,"throttleStatus":{"maximumAvailable":2000,"currentlyAvailable":50,"restoreRate":100}}}}`))
		}, provider.KindRateLimited, 2 * time.Second},
		{"http 503", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, provider.KindUnavailable, 0},
		{"http 401", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":"[API] Invalid API key or access token"}`))
		}, provider.KindUnauthorized, 0},
		{"access denied", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"errors":[{"message":"Access denied","extensions":{"code":"ACCESS_DENIED"}}]}`))
		}, provider.KindUnauthorized, 0},
		{"malformed json", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":`))
		}, provider.KindBadResponse, 0},
		{"no data", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}, provider.KindBadResponse, 0},
		{"id mismatch", func(w http.ResponseWriter, _ *http.Request) {
			n := shopifytest.ToNode(fixtures.Orders()[1]) // order 1002
			_ = json.NewEncoder(w).Encode(shopify.GraphQLResponse{Data: &shopify.OrderData{Order: n}})
		}, provider.KindBadResponse, 0},
		{"incomplete order", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"order":{"id":"gid://shopify/Order/1","name":"#1","createdAt":"","totalPriceSet":{"shopMoney":{"amount":"1.00","currencyCode":"USD"}}}}}`))
		}, provider.KindBadResponse, 0},
		{"oversized body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"order":null},"pad":"` + strings.Repeat("x", 2<<20) + `"}`))
		}, provider.KindBadResponse, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, tc.handler)
			id := "1001"
			if tc.name == "incomplete order" {
				id = "1"
			}
			_, err := c.GetOrder(context.Background(), id)
			pe := wantKind(t, err, tc.kind)
			if pe.RetryAfter != tc.retry {
				t.Fatalf("RetryAfter = %v, want %v", pe.RetryAfter, tc.retry)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("error message leaks the access token")
			}
		})
	}
}

func TestGetOrderTimeout(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.GetOrder(ctx, "1001")
	wantKind(t, err, provider.KindTimeout)
}

func TestGetOrderCallerCancellationIsNotAnUpstreamFault(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := c.GetOrder(ctx, "1001")
	if !errors.Is(err, context.Canceled) || provider.KindOf(err) != provider.KindUnknown {
		t.Fatalf("err = %v, want bare context.Canceled", err)
	}
}

func TestGetOrderConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := shopify.New(shopify.Config{AccessToken: testToken, Endpoint: url, AllowInsecureEndpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetOrder(context.Background(), "1001")
	wantKind(t, err, provider.KindUnavailable)
}

func TestNewValidatesConfig(t *testing.T) {
	cases := map[string]shopify.Config{
		"missing token":    {ShopDomain: "acme.myshopify.com", APIVersion: "2025-07"},
		"bad domain":       {AccessToken: "x", ShopDomain: "evil.example.com", APIVersion: "2025-07"},
		"bad version":      {AccessToken: "x", ShopDomain: "acme.myshopify.com", APIVersion: "latest"},
		"insecure":         {AccessToken: "x", Endpoint: "http://example.com/graphql.json"},
		"invalid endpoint": {AccessToken: "x", Endpoint: "::not a url"},
	}
	for name, cfg := range cases {
		if _, err := shopify.New(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := shopify.New(shopify.Config{AccessToken: "x", ShopDomain: "acme.myshopify.com", APIVersion: "2025-07"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestUnknownEnumsAreNotInvented(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"order":{"id":"gid://shopify/Order/5","name":"#5","createdAt":"2026-01-01T00:00:00Z",
			"displayFinancialStatus":"SOMETHING_NEW","displayFulfillmentStatus":"TELEPORTED",
			"totalPriceSet":{"shopMoney":{"amount":"1.00","currencyCode":"USD"}},"lineItems":{"nodes":[]},"fulfillments":[]}}}`))
	}))
	o, err := c.GetOrder(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if o.FinancialStatus != provider.FinancialUnknown || o.FulfillmentStatus != provider.FulfillmentUnknown {
		t.Fatalf("statuses = %q/%q, want unknown/unknown", o.FinancialStatus, o.FulfillmentStatus)
	}
}

func TestListOrders(t *testing.T) {
	c := newClient(t, shopifytest.NewServer(testToken, fixtures.Orders()))
	ctx := context.Background()

	alice, err := c.ListOrders(ctx, provider.ListFilter{CustomerID: fixtures.CustomerAlice})
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 2 || alice[0].ID != "1003" || alice[1].ID != "1001" {
		t.Fatalf("alice's orders = %v", orderIDs(alice))
	}
	all, err := c.ListOrders(ctx, provider.ListFilter{Limit: 3})
	if err != nil || len(all) != 3 {
		t.Fatalf("all (limit 3) = %v, %v", orderIDs(all), err)
	}
	if _, err := c.ListOrders(ctx, provider.ListFilter{CustomerID: "7001 OR status:any"}); err == nil {
		t.Fatal("search syntax injection must be rejected")
	}
}

func TestListOrdersSendsSearchQuery(t *testing.T) {
	var got shopify.GraphQLRequest
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"data":{"orders":{"nodes":[]}}}`))
	}))
	if _, err := c.ListOrders(context.Background(), provider.ListFilter{CustomerID: "7002", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if got.Variables["query"] != "customer_id:7002" || got.Variables["first"] != float64(provider.MaxListLimit) ||
		!strings.Contains(got.Query, "orders(first: $first, query: $query") {
		t.Fatalf("request = %+v", got)
	}
}

func TestListOrdersRejectsOtherCustomersOrders(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := shopifytest.ToNode(fixtures.Orders()[1]) // Bob's order
		_ = json.NewEncoder(w).Encode(shopify.GraphQLResponse{Data: &shopify.OrderData{Orders: &shopify.OrderConnection{Nodes: []shopify.OrderNode{*n}}}})
	}))
	_, err := c.ListOrders(context.Background(), provider.ListFilter{CustomerID: fixtures.CustomerAlice})
	wantKind(t, err, provider.KindBadResponse)
}

func orderIDs(os []*provider.Order) []string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.ID)
	}
	return out
}
