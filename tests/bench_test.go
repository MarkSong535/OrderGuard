package tests

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"orderguard/internal/api"
	"orderguard/internal/auth"
	"orderguard/internal/observability"
	"orderguard/internal/orders"
	"orderguard/internal/provider/fixtures"
	"orderguard/internal/provider/mock"
)

// BenchmarkHandlerInProcess measures OrderGuard's own overhead (routing,
// auth, resilience envelope, JSON encoding) with an in-memory provider and
// no network.
func BenchmarkHandlerInProcess(b *testing.B) {
	key, _ := auth.GenerateKey()
	authn, err := auth.NewAuthenticator([]auth.KeyEntry{{ID: "support", KeySHA256: auth.HashKey(key), Role: auth.RoleSupport}})
	if err != nil {
		b.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	m := observability.NewMetrics()
	svc := orders.NewService(mock.New(fixtures.Orders()), defaultOrdersConfig(), log, m)
	h := api.NewHandler(api.Config{}, svc, authn, log, m)

	ids := []string{"1001", "1002", "1003", "1004"}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			r := httptest.NewRequest(http.MethodGet, "/orders/"+ids[i%len(ids)], nil)
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				b.Fatalf("status %d", w.Code)
			}
			i++
		}
	})
}

// BenchmarkEndToEndLoopback measures the full path over real sockets:
// client → OrderGuard → Shopify GraphQL client → fake Shopify.
func BenchmarkEndToEndLoopback(b *testing.B) {
	s := newStack(b)
	ids := []string{"1001", "1002", "1003", "1004"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if r := s.get("support-bot", "/orders/"+ids[i%len(ids)]); r.status != http.StatusOK {
				b.Fatalf("status %d", r.status)
			}
			i++
		}
	})
	b.ReportMetric(float64(s.shopify.Requests())/float64(b.N), "upstream/op")
}
