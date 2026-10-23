package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestLoggerAddsRequestID(t *testing.T) {
	var buf bytes.Buffer
	log, err := NewLogger(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.With("component", "test").InfoContext(WithRequestID(context.Background(), "req-1"), "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["request_id"] != "req-1" || rec["component"] != "test" || rec["msg"] != "hello" {
		t.Fatalf("record = %v", rec)
	}
}

func TestNewLoggerRejectsBadConfig(t *testing.T) {
	if _, err := NewLogger(&bytes.Buffer{}, "loud", "json"); err == nil {
		t.Error("expected error for bad level")
	}
	if _, err := NewLogger(&bytes.Buffer{}, "info", "xml"); err == nil {
		t.Error("expected error for bad format")
	}
}

func TestRequestIDValidation(t *testing.T) {
	if !ValidRequestID(NewRequestID()) || !ValidRequestID("abc-123_X.y") {
		t.Fatal("valid IDs rejected")
	}
	for _, bad := range []string{"", "has space", "new\nline", strings.Repeat("a", 65), `quote"`} {
		if ValidRequestID(bad) {
			t.Errorf("ValidRequestID(%q) = true", bad)
		}
	}
}

func TestPrometheusExposition(t *testing.T) {
	r := &Registry{}
	c := r.NewCounterVec("test_total", "A counter.", "path")
	g := r.NewGaugeVec("test_gauge", "A gauge.")
	h := r.NewHistogramVec("test_seconds", "A histogram.", []float64{0.1, 1}, "op")

	c.Inc(`/a"b`)
	c.Add(2, "/c")
	g.Set(5)
	g.Add(-2)
	h.Observe(0.05, "get")
	h.Observe(0.5, "get")
	h.Observe(5, "get")

	var buf bytes.Buffer
	r.Write(&buf)
	out := buf.String()
	for _, want := range []string{
		"# TYPE test_total counter",
		`test_total{path="/a\"b"} 1`,
		`test_total{path="/c"} 2`,
		"test_gauge 3",
		`test_seconds_bucket{op="get",le="0.1"} 1`,
		`test_seconds_bucket{op="get",le="1"} 2`,
		`test_seconds_bucket{op="get",le="+Inf"} 3`,
		`test_seconds_sum{op="get"} 5.55`,
		`test_seconds_count{op="get"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestMetricsConcurrentUpdates(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 1000 {
				m.HTTPRequests.Inc("GET", "/x", "200")
				m.HTTPDuration.Observe(0.01, "/x")
				m.HTTPInFlight.Add(1)
				m.HTTPInFlight.Add(-1)
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			m.Registry.Write(&bytes.Buffer{})
		}
	})
	wg.Wait()
	if v := m.HTTPRequests.Value("GET", "/x", "200"); v != 32000 {
		t.Fatalf("counter = %v, want 32000", v)
	}
	if m.HTTPInFlight.Value() != 0 {
		t.Fatal("gauge drifted")
	}
}
