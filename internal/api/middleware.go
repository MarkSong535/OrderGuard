package api

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"

	"orderguard/internal/auth"
	"orderguard/internal/observability"
)

// reqInfo is filled in by inner handlers and read by the access logger,
// which runs outside the mux and so cannot see the matched route directly.
type reqInfo struct {
	route     string
	principal string
}

type reqInfoKey struct{}

func infoFrom(r *http.Request) *reqInfo {
	if ri, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{}
}

// statusRecorder captures the status code written by a handler.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// requestID assigns or propagates X-Request-ID.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !observability.ValidRequestID(id) {
			id = observability.NewRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := observability.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLog records one structured log line and request metrics per request.
func accessLog(log *slog.Logger, m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ri := &reqInfo{route: "unmatched"}
			r = r.WithContext(contextWithInfo(r, ri))
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			m.HTTPInFlight.Add(1)
			defer m.HTTPInFlight.Add(-1)
			next.ServeHTTP(rec, r)

			elapsed := time.Since(start)
			m.HTTPRequests.Inc(r.Method, ri.route, strconv.Itoa(rec.status))
			m.HTTPDuration.Observe(elapsed.Seconds(), ri.route)

			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			} else if rec.status >= 400 {
				level = slog.LevelWarn
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("route", ri.route),
				slog.Int("status", rec.status),
				slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
				slog.String("remote_addr", r.RemoteAddr),
			}
			if ri.principal != "" {
				attrs = append(attrs, slog.String("principal", ri.principal))
			}
			log.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

// recoverer turns handler panics into 500 responses instead of dropping the
// connection, and logs the stack.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.ErrorContext(r.Context(), "panic serving request", "panic", v, "stack", string(debug.Stack()))
					writeError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeaders applies conservative defaults for a JSON API that serves
// personal data.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// loadShed rejects requests beyond max in-flight with 503 rather than letting
// latency grow without bound. max <= 0 disables shedding.
func loadShed(max int64, m *observability.Metrics) func(http.Handler) http.Handler {
	var inFlight atomic.Int64
	return func(next http.Handler) http.Handler {
		if max <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if inFlight.Add(1) > max {
				inFlight.Add(-1)
				m.HTTPShed.Inc()
				w.Header().Set("Retry-After", "1")
				writeError(w, r, http.StatusServiceUnavailable, "overloaded", "server is at capacity, retry shortly")
				return
			}
			defer inFlight.Add(-1)
			next.ServeHTTP(w, r)
		})
	}
}

// authenticate requires a valid bearer API key.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := auth.BearerToken(r)
		var p auth.Principal
		if err == nil {
			p, err = s.auth.Authenticate(token)
		}
		if err != nil {
			reason := "invalid"
			if errors.Is(err, auth.ErrMissingCredentials) {
				reason = "missing"
			}
			s.metrics.AuthFailures.Inc(reason)
			w.Header().Set("WWW-Authenticate", `Bearer realm="orderguard"`)
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
			return
		}
		infoFrom(r).principal = p.ID
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// rateLimit applies the per-principal token bucket.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		if ok, retryAfter := s.limiter.Allow(p.ID); !ok {
			s.metrics.RateLimited.Inc()
			setRetryAfter(w, retryAfter)
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "request rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// methods restricts a handler to the given methods with a JSON 405.
func methods(allowed string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != allowed && !(allowed == http.MethodGet && r.Method == http.MethodHead) {
			w.Header().Set("Allow", allowed)
			writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		next(w, r)
	})
}
