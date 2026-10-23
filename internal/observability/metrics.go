package observability

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metrics and renders them in the Prometheus text exposition
// format. It is intentionally small: counters, gauges and histograms with
// string labels are all this service needs.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
}

type metric interface {
	write(w io.Writer)
}

func (r *Registry) register(m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, m)
}

// Write renders every registered metric.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	ms := append([]metric(nil), r.metrics...)
	r.mu.Unlock()
	bw := bufio.NewWriter(w)
	for _, m := range ms {
		m.write(bw)
	}
	_ = bw.Flush()
}

// Handler serves the registry at a /metrics endpoint.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

type desc struct {
	name   string
	help   string
	kind   string
	labels []string
}

func (d desc) header(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", d.name, d.help, d.name, d.kind)
}

func (d desc) key(values []string) string {
	if len(values) != len(d.labels) {
		panic(fmt.Sprintf("metric %s: got %d label values, want %d", d.name, len(values), len(d.labels)))
	}
	return strings.Join(values, "\xff")
}

func formatLabels(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	pairs := make([]string, 0, len(names)+len(extra)/2)
	for i, n := range names {
		pairs = append(pairs, n+`="`+escapeLabel(values[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		pairs = append(pairs, extra[i]+`="`+escapeLabel(extra[i+1])+`"`)
	}
	b.WriteString(strings.Join(pairs, ","))
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(v string) string { return labelEscaper.Replace(v) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// CounterVec is a monotonically increasing counter partitioned by labels.
type CounterVec struct {
	desc
	mu     sync.Mutex
	values map[string]*counterEntry
}

type counterEntry struct {
	labels []string
	value  float64
}

// NewCounterVec creates and registers a counter.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{desc: desc{name, help, "counter", labels}, values: map[string]*counterEntry{}}
	r.register(c)
	return c
}

// Inc adds one to the counter identified by labelValues.
func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds v (which must be non-negative) to the counter.
func (c *CounterVec) Add(v float64, labelValues ...string) {
	if v < 0 {
		panic("counter cannot decrease")
	}
	k := c.key(labelValues)
	c.mu.Lock()
	e, ok := c.values[k]
	if !ok {
		e = &counterEntry{labels: append([]string(nil), labelValues...)}
		c.values[k] = e
	}
	e.value += v
	c.mu.Unlock()
}

// Value returns the current value; it is intended for tests.
func (c *CounterVec) Value(labelValues ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.values[c.key(labelValues)]; ok {
		return e.value
	}
	return 0
}

func (c *CounterVec) write(w io.Writer) {
	c.header(w)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range sortedKeys(c.values) {
		e := c.values[k]
		fmt.Fprintf(w, "%s%s %s\n", c.name, formatLabels(c.labels, e.labels), formatFloat(e.value))
	}
}

// GaugeVec is a value that can go up and down, partitioned by labels.
type GaugeVec struct {
	desc
	mu     sync.Mutex
	values map[string]*counterEntry
}

// NewGaugeVec creates and registers a gauge.
func (r *Registry) NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	g := &GaugeVec{desc: desc{name, help, "gauge", labels}, values: map[string]*counterEntry{}}
	r.register(g)
	return g
}

// Set sets the gauge to v.
func (g *GaugeVec) Set(v float64, labelValues ...string) {
	g.update(labelValues, func(float64) float64 { return v })
}

// Add adds delta (which may be negative) to the gauge.
func (g *GaugeVec) Add(delta float64, labelValues ...string) {
	g.update(labelValues, func(cur float64) float64 { return cur + delta })
}

// Value returns the current value; it is intended for tests.
func (g *GaugeVec) Value(labelValues ...string) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.values[g.key(labelValues)]; ok {
		return e.value
	}
	return 0
}

func (g *GaugeVec) update(labelValues []string, f func(float64) float64) {
	k := g.key(labelValues)
	g.mu.Lock()
	e, ok := g.values[k]
	if !ok {
		e = &counterEntry{labels: append([]string(nil), labelValues...)}
		g.values[k] = e
	}
	e.value = f(e.value)
	g.mu.Unlock()
}

func (g *GaugeVec) write(w io.Writer) {
	g.header(w)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range sortedKeys(g.values) {
		e := g.values[k]
		fmt.Fprintf(w, "%s%s %s\n", g.name, formatLabels(g.labels, e.labels), formatFloat(e.value))
	}
}

// HistogramVec samples observations into cumulative buckets.
type HistogramVec struct {
	desc
	buckets []float64
	mu      sync.Mutex
	values  map[string]*histEntry
}

type histEntry struct {
	labels []string
	counts []uint64 // non-cumulative, one per bucket
	sum    float64
	count  uint64
}

// DefaultLatencyBuckets covers 1ms to 10s, in seconds.
var DefaultLatencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// NewHistogramVec creates and registers a histogram with the given upper bounds.
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	h := &HistogramVec{desc: desc{name, help, "histogram", labels}, buckets: b, values: map[string]*histEntry{}}
	r.register(h)
	return h
}

// Observe records v.
func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	k := h.key(labelValues)
	idx := sort.SearchFloat64s(h.buckets, v) // first bucket with bound >= v
	h.mu.Lock()
	e, ok := h.values[k]
	if !ok {
		e = &histEntry{labels: append([]string(nil), labelValues...), counts: make([]uint64, len(h.buckets))}
		h.values[k] = e
	}
	if idx < len(e.counts) {
		e.counts[idx]++
	}
	e.sum += v
	e.count++
	h.mu.Unlock()
}

// Count returns the number of observations; it is intended for tests.
func (h *HistogramVec) Count(labelValues ...string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.values[h.key(labelValues)]; ok {
		return e.count
	}
	return 0
}

func (h *HistogramVec) write(w io.Writer) {
	h.header(w)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range sortedKeys(h.values) {
		e := h.values[k]
		var cum uint64
		for i, bound := range h.buckets {
			cum += e.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, formatLabels(h.labels, e.labels, "le", formatFloat(bound)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, formatLabels(h.labels, e.labels, "le", "+Inf"), e.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, formatLabels(h.labels, e.labels), formatFloat(e.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, formatLabels(h.labels, e.labels), e.count)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Metrics is the set of application metrics exported by OrderGuard.
type Metrics struct {
	Registry *Registry

	HTTPRequests *CounterVec   // method, route, status
	HTTPDuration *HistogramVec // route
	HTTPInFlight *GaugeVec
	HTTPShed     *CounterVec

	UpstreamRequests *CounterVec   // provider, outcome
	UpstreamDuration *HistogramVec // provider
	UpstreamRetries  *CounterVec   // provider

	BreakerState       *GaugeVec   // name; 0=closed 1=half_open 2=open
	BreakerTransitions *CounterVec // name, to

	AuthFailures *CounterVec // reason
	AuthzDenied  *CounterVec
	RateLimited  *CounterVec
	StaleServed  *CounterVec
	Deduplicated *CounterVec
}

// NewMetrics creates the application metrics on a fresh registry.
func NewMetrics() *Metrics {
	r := &Registry{}
	return &Metrics{
		Registry:     r,
		HTTPRequests: r.NewCounterVec("orderguard_http_requests_total", "HTTP requests by method, route and status code.", "method", "route", "status"),
		HTTPDuration: r.NewHistogramVec("orderguard_http_request_duration_seconds", "HTTP request latency.", DefaultLatencyBuckets, "route"),
		HTTPInFlight: r.NewGaugeVec("orderguard_http_requests_in_flight", "HTTP requests currently being served."),
		HTTPShed:     r.NewCounterVec("orderguard_http_requests_shed_total", "Requests rejected because the server was at capacity."),

		UpstreamRequests: r.NewCounterVec("orderguard_upstream_requests_total", "Upstream provider calls by outcome.", "provider", "outcome"),
		UpstreamDuration: r.NewHistogramVec("orderguard_upstream_request_duration_seconds", "Upstream provider call latency per attempt.", DefaultLatencyBuckets, "provider"),
		UpstreamRetries:  r.NewCounterVec("orderguard_upstream_retries_total", "Upstream call retries.", "provider"),

		BreakerState:       r.NewGaugeVec("orderguard_circuit_breaker_state", "Circuit breaker state (0=closed, 1=half_open, 2=open).", "name"),
		BreakerTransitions: r.NewCounterVec("orderguard_circuit_breaker_transitions_total", "Circuit breaker state transitions.", "name", "to"),

		AuthFailures: r.NewCounterVec("orderguard_auth_failures_total", "Authentication failures by reason.", "reason"),
		AuthzDenied:  r.NewCounterVec("orderguard_authz_denied_total", "Order accesses denied by authorization policy."),
		RateLimited:  r.NewCounterVec("orderguard_rate_limited_total", "Requests rejected by the per-client rate limiter."),
		StaleServed:  r.NewCounterVec("orderguard_stale_responses_total", "Responses served from the last-known-good cache during upstream failure."),
		Deduplicated: r.NewCounterVec("orderguard_deduplicated_requests_total", "Order lookups that joined an in-flight upstream call."),
	}
}
