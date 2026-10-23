// Package shopifytest provides a fake Shopify Admin GraphQL endpoint with
// fault injection. It backs the integration tests and cmd/mockshopify.
package shopifytest

import (
	"crypto/subtle"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orderguard/internal/provider"
	"orderguard/internal/provider/shopify"
)

// Faults controls injected misbehaviour.
type Faults struct {
	Latency            time.Duration `json:"-"`
	FailFirst          int           `json:"fail_first"`  // fail the next N requests
	FailStatus         int           `json:"fail_status"` // HTTP status for failures (default 503)
	FailRate           float64       `json:"fail_rate"`   // probability of failure, 0..1
	Throttle           bool          `json:"throttle"`    // answer with a GraphQL THROTTLED error
	RateLimitHTTP      bool          `json:"rate_limit_http"`
	RetryAfterSeconds  int           `json:"retry_after_seconds"`
	MalformedResponses bool          `json:"malformed_responses"`
}

// Server is an http.Handler emulating the Shopify order query.
type Server struct {
	token string

	mu            sync.Mutex
	orders        map[string]provider.Order
	faults        Faults
	failRemaining int

	requests atomic.Int64
}

// NewServer returns a fake that accepts the given access token.
func NewServer(token string, orders []provider.Order) *Server {
	s := &Server{token: token, orders: make(map[string]provider.Order, len(orders))}
	for _, o := range orders {
		s.orders[o.ID] = o
	}
	return s
}

// SetFaults replaces the active faults and resets the FailFirst counter.
func (s *Server) SetFaults(f Faults) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = f
	s.failRemaining = f.FailFirst
}

// Faults returns the active faults.
func (s *Server) Faults() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.faults
}

// Requests returns the number of GraphQL requests received.
func (s *Server) Requests() int64 { return s.requests.Load() }

// ResetRequests zeroes the request counter.
func (s *Server) ResetRequests() { s.requests.Store(0) }

type decision int

const (
	decideOK decision = iota
	decideFail
	decideThrottle
	decideRateLimit
	decideMalformed
)

func (s *Server) decide() (Faults, decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.faults
	switch {
	case s.failRemaining > 0:
		s.failRemaining--
		return f, decideFail
	case f.FailRate > 0 && rand.Float64() < f.FailRate:
		return f, decideFail
	case f.RateLimitHTTP:
		return f, decideRateLimit
	case f.Throttle:
		return f, decideThrottle
	case f.MalformedResponses:
		return f, decideMalformed
	}
	return f, decideOK
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Shopify-Access-Token")), []byte(s.token)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"errors": "[API] Invalid API key or access token (unrecognized login or wrong password)",
		})
		return
	}
	s.requests.Add(1)

	// Read the body before any injected latency: net/http only detects a
	// client disconnect once the request body has been consumed.
	var req shopify.GraphQLRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"errors": "invalid JSON"})
		return
	}

	f, d := s.decide()
	if f.Latency > 0 {
		t := time.NewTimer(f.Latency)
		select {
		case <-r.Context().Done():
			t.Stop()
			return
		case <-t.C:
		}
	}

	switch d {
	case decideFail:
		status := f.FailStatus
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", strconv.Itoa(max(f.RetryAfterSeconds, 1)))
		}
		writeJSON(w, status, map[string]string{"errors": "injected failure"})
		return
	case decideRateLimit:
		w.Header().Set("Retry-After", strconv.Itoa(max(f.RetryAfterSeconds, 1)))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"errors": "Exceeded rate limit"})
		return
	case decideThrottle:
		writeJSON(w, http.StatusOK, shopify.GraphQLResponse{
			Errors: shopify.GraphQLErrors{throttledError()},
			Extensions: &shopify.ResponseExtras{Cost: &shopify.QueryCost{
				RequestedQueryCost: 12,
				ThrottleStatus:     shopify.ThrottleStatus{MaximumAvailable: 2000, CurrentlyAvailable: 2, RestoreRate: 100},
			}},
		})
		return
	case decideMalformed:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"order":{"id":`))
		return
	}

	if _, single := req.Variables["id"]; !single {
		s.serveList(w, req)
		return
	}
	gid, _ := req.Variables["id"].(string)
	id, err := shopify.ParseOrderGID(gid)
	if err != nil {
		writeJSON(w, http.StatusOK, shopify.GraphQLResponse{Errors: shopify.GraphQLErrors{{Message: "Invalid global id '" + gid + "'"}}})
		return
	}

	s.mu.Lock()
	o, ok := s.orders[id]
	s.mu.Unlock()

	resp := shopify.GraphQLResponse{Data: &shopify.OrderData{}, Extensions: okCost()}
	if ok {
		resp.Data.Order = ToNode(o)
	}
	writeJSON(w, http.StatusOK, resp)
}

// serveList answers the ListOrders query: newest first, optional
// "customer_id:<id>" filter, at most `first` orders.
func (s *Server) serveList(w http.ResponseWriter, req shopify.GraphQLRequest) {
	first := 50
	if f, ok := req.Variables["first"].(float64); ok && f >= 1 && f <= 250 {
		first = int(f)
	}
	query, _ := req.Variables["query"].(string)
	customer, filtered := strings.CutPrefix(query, "customer_id:")
	if query != "" && !filtered {
		writeJSON(w, http.StatusOK, shopify.GraphQLResponse{Errors: shopify.GraphQLErrors{{Message: "unsupported search query in fake server"}}})
		return
	}

	s.mu.Lock()
	matched := make([]provider.Order, 0, len(s.orders))
	for _, o := range s.orders {
		if !filtered || o.CustomerID == customer {
			matched = append(matched, o)
		}
	}
	s.mu.Unlock()
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt.After(matched[j].CreatedAt) })
	if len(matched) > first {
		matched = matched[:first]
	}

	conn := &shopify.OrderConnection{Nodes: make([]shopify.OrderNode, 0, len(matched))}
	for _, o := range matched {
		conn.Nodes = append(conn.Nodes, *ToNode(o))
	}
	writeJSON(w, http.StatusOK, shopify.GraphQLResponse{Data: &shopify.OrderData{Orders: conn}, Extensions: okCost()})
}

func throttledError() shopify.GraphQLError {
	e := shopify.GraphQLError{Message: "Throttled"}
	e.Extensions.Code = "THROTTLED"
	return e
}

func okCost() *shopify.ResponseExtras {
	actual := 7.0
	return &shopify.ResponseExtras{Cost: &shopify.QueryCost{
		RequestedQueryCost: 12, ActualQueryCost: &actual,
		ThrottleStatus: shopify.ThrottleStatus{MaximumAvailable: 2000, CurrentlyAvailable: 1993, RestoreRate: 100},
	}}
}

// ToNode renders a normalized order in Shopify's wire format.
func ToNode(o provider.Order) *shopify.OrderNode {
	upper := func(s string) *string {
		if s == "" {
			return nil
		}
		u := toUpper(s)
		return &u
	}
	strp := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	n := &shopify.OrderNode{
		ID:                       shopify.OrderGID(o.ID),
		Name:                     o.Name,
		Email:                    strp(o.Email),
		CreatedAt:                o.CreatedAt.Format(time.RFC3339),
		UpdatedAt:                o.UpdatedAt.Format(time.RFC3339),
		DisplayFinancialStatus:   upper(string(o.FinancialStatus)),
		DisplayFulfillmentStatus: toUpper(string(o.FulfillmentStatus)),
		TotalPriceSet:            shopify.MoneyBag{ShopMoney: shopify.MoneyV2{Amount: o.Total.Amount, CurrencyCode: o.Total.Currency}},
		Fulfillments:             []shopify.FulfillmentNd{},
	}
	if o.CustomerID != "" {
		n.Customer = &shopify.CustomerNode{ID: shopify.CustomerGID(o.CustomerID)}
	}
	for _, li := range o.LineItems {
		n.LineItems.Nodes = append(n.LineItems.Nodes, shopify.LineItemNode{Title: li.Title, SKU: strp(li.SKU), Quantity: li.Quantity})
	}
	for _, f := range o.Fulfillments {
		fn := shopify.FulfillmentNd{
			Status:        toUpper(string(f.Status)),
			DisplayStatus: upper(f.DisplayStatus),
			CreatedAt:     f.CreatedAt.Format(time.RFC3339),
			UpdatedAt:     f.UpdatedAt.Format(time.RFC3339),
			TrackingInfo:  []shopify.TrackingNode{},
		}
		for _, t := range f.Tracking {
			fn.TrackingInfo = append(fn.TrackingInfo, shopify.TrackingNode{Company: strp(t.Company), Number: strp(t.Number), URL: strp(t.URL)})
		}
		n.Fulfillments = append(n.Fulfillments, fn)
	}
	return n
}

func toUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// AdminHandler exposes GET/PUT /faults and GET /stats for interactive demos.
// It is never mounted on the OrderGuard server itself.
func (s *Server) AdminHandler() http.Handler {
	type faultsJSON struct {
		Faults
		LatencyMS int64 `json:"latency_ms"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /faults", func(w http.ResponseWriter, _ *http.Request) {
		f := s.Faults()
		writeJSON(w, http.StatusOK, faultsJSON{Faults: f, LatencyMS: f.Latency.Milliseconds()})
	})
	mux.HandleFunc("PUT /faults", func(w http.ResponseWriter, r *http.Request) {
		var in faultsJSON
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		in.Faults.Latency = time.Duration(in.LatencyMS) * time.Millisecond
		s.SetFaults(in.Faults)
		writeJSON(w, http.StatusOK, in)
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int64{"graphql_requests": s.Requests()})
	})
	mux.HandleFunc("POST /stats/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.ResetRequests()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
