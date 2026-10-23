// Package api is the HTTP/JSON layer: routing, middleware, authentication
// and the mapping of domain errors to status codes.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"orderguard/internal/auth"
	"orderguard/internal/observability"
	"orderguard/internal/orders"
	"orderguard/internal/resilience"
)

// OrderService is the subset of *orders.Service the API depends on.
type OrderService interface {
	GetOrder(ctx context.Context, p auth.Principal, id string) (*orders.Result, error)
	SearchOrders(ctx context.Context, p auth.Principal, query string, limit int) (*orders.SearchResult, error)
	Health() orders.Health
}

// Config holds HTTP-layer settings.
type Config struct {
	MaxInFlight    int64   // load-shedding threshold; 0 disables
	RateLimitRPS   float64 // per-principal sustained rate; 0 disables
	RateLimitBurst int
	// Mount registers additional routes (for example the interactive demo)
	// behind the same middleware chain.
	Mount func(route func(pattern string, h http.Handler))
}

// Server holds dependencies for the HTTP handlers.
type Server struct {
	orders  OrderService
	auth    *auth.Authenticator
	limiter *resilience.RateLimiter
	log     *slog.Logger
	metrics *observability.Metrics
	started time.Time
}

// NewHandler returns the fully wired HTTP handler.
func NewHandler(cfg Config, svc OrderService, a *auth.Authenticator, log *slog.Logger, m *observability.Metrics) http.Handler {
	s := &Server{
		orders:  svc,
		auth:    a,
		limiter: resilience.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst),
		log:     log,
		metrics: m,
		started: time.Now(),
	}

	mux := http.NewServeMux()
	route := func(pattern string, h http.Handler) {
		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			infoFrom(r).route = pattern
			h.ServeHTTP(w, r)
		}))
	}
	protected := func(h http.Handler) http.Handler { return s.authenticate(s.rateLimit(h)) }

	route("/health", methods(http.MethodGet, s.handleHealth))
	route("/metrics", methods(http.MethodGet, m.Registry.Handler().ServeHTTP))
	route("/orders", protected(methods(http.MethodGet, s.handleSearch)))
	route("/orders/{id}", protected(methods(http.MethodGet, s.handleGetOrder)))
	route("/orders/{id}/fulfillment", protected(methods(http.MethodGet, s.handleGetFulfillment)))
	route("/mcp", protected(http.HandlerFunc(s.handleMCP)))
	if cfg.Mount != nil {
		cfg.Mount(route)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no such endpoint")
	})

	var h http.Handler = mux
	h = loadShed(cfg.MaxInFlight, m)(h)
	h = securityHeaders(h)
	h = recoverer(log)(h)
	h = accessLog(log, m)(h)
	h = requestID(h)
	return h
}

func contextWithInfo(r *http.Request, ri *reqInfo) context.Context {
	return context.WithValue(r.Context(), reqInfoKey{}, ri)
}

type healthResponse struct {
	Status        string            `json:"status"` // "ok" or "degraded"
	UptimeSeconds int64             `json:"uptime_seconds"`
	Checks        map[string]string `json:"checks"`
}

// handleHealth is a liveness check that also reports dependency state. It
// returns 200 while degraded: the process is healthy and can still serve
// cached data, so orchestrators should not restart it.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := s.orders.Health()
	status := "ok"
	if h.BreakerState != resilience.StateClosed {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Status:        status,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Checks: map[string]string{
			"provider":        h.Provider,
			"circuit_breaker": h.BreakerState.String(),
		},
	})
}

func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	res, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, OrderResponse{
		Order: toOrderDTO(res.Order),
		Meta:  toMeta(res, observability.RequestID(r.Context())),
	})
}

func (s *Server) handleGetFulfillment(w http.ResponseWriter, r *http.Request) {
	res, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, FulfillmentResponse{
		OrderID:           res.Order.ID,
		Name:              res.Order.Name,
		FulfillmentStatus: string(res.Order.FulfillmentStatus),
		Fulfillments:      toFulfillmentDTOs(res.Order.Fulfillments),
		Meta:              toMeta(res, observability.RequestID(r.Context())),
	})
}

// lookup runs the order query and writes an error response on failure.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*orders.Result, bool) {
	p, _ := auth.PrincipalFrom(r.Context())
	res, err := s.orders.GetOrder(r.Context(), p, r.PathValue("id"))
	if err != nil {
		s.writeOrderError(w, r, err)
		return nil, false
	}
	if res.Source == orders.SourceStale {
		w.Header().Set("Warning", `110 orderguard "Response is stale"`)
	}
	return res, true
}

// orderError is the client-facing description of a service error.
type orderError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func describeOrderError(err error) orderError {
	var retryAfter time.Duration
	if oe := (*orders.Error)(nil); errors.As(err, &oe) {
		retryAfter = oe.RetryAfter
	}
	switch orders.KindOf(err) {
	case orders.KindInvalidID:
		return orderError{http.StatusBadRequest, "invalid_order_id", "order id must be a positive integer", 0}
	case orders.KindInvalidQuery:
		return orderError{http.StatusBadRequest, "invalid_query", "query must be a valid regular expression of at most 200 characters", 0}
	case orders.KindNotFound:
		return orderError{http.StatusNotFound, "order_not_found", "order not found", 0}
	case orders.KindUnavailable:
		return orderError{http.StatusServiceUnavailable, "upstream_unavailable", "order provider is temporarily unavailable", retryAfter}
	case orders.KindTimeout:
		return orderError{http.StatusGatewayTimeout, "upstream_timeout", "order provider did not respond in time", 0}
	case orders.KindUpstream:
		return orderError{http.StatusBadGateway, "upstream_error", "order provider returned an invalid response", 0}
	case orders.KindCanceled:
		return orderError{499, "canceled", "request canceled", 0} // de facto status for a client that went away
	}
	return orderError{http.StatusInternalServerError, "internal_error", "internal server error", 0}
}

func (s *Server) logOrderError(r *http.Request, err error) {
	kind := orders.KindOf(err)
	switch kind {
	case orders.KindInternal, orders.KindUpstream:
		s.log.ErrorContext(r.Context(), "order lookup failed", "kind", kind.String(), "error", err)
	case orders.KindNotFound, orders.KindInvalidID, orders.KindInvalidQuery:
	default:
		s.log.WarnContext(r.Context(), "order lookup failed", "kind", kind.String(), "error", err)
	}
}

func (s *Server) writeOrderError(w http.ResponseWriter, r *http.Request, err error) {
	s.logOrderError(r, err)
	e := describeOrderError(err)
	if e.Status == 499 {
		w.WriteHeader(499)
		return
	}
	setRetryAfter(w, e.RetryAfter)
	writeError(w, r, e.Status, e.Code, e.Message)
}

// handleSearch serves GET /orders?q=<regex>&limit=<n>.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 50 {
			writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be an integer between 1 and 50")
			return
		}
		limit = n
	}
	res, err := s.orders.SearchOrders(r.Context(), p, r.URL.Query().Get("q"), limit)
	if err != nil {
		s.writeOrderError(w, r, err)
		return
	}
	if res.Source == orders.SourceStale {
		w.Header().Set("Warning", `110 orderguard "Response is stale"`)
	}
	writeJSON(w, http.StatusOK, toSearchResponse(res, observability.RequestID(r.Context())))
}
