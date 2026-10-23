// Command mockshopify runs a fake Shopify Admin GraphQL endpoint serving
// fixture orders, with runtime fault injection for demos:
//
//	PUT  /_admin/faults   {"latency_ms":3000,"fail_first":2,"fail_status":503,"throttle":false}
//	GET  /_admin/faults
//	GET  /_admin/stats
//	POST /_admin/stats/reset
//
// It is a development tool and must never be exposed publicly.
package main

import (
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"orderguard/internal/provider/fixtures"
	"orderguard/internal/provider/shopify/shopifytest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "listen address")
	flag.Parse()

	token := os.Getenv("MOCKSHOPIFY_TOKEN")
	if token == "" {
		log.Fatal("MOCKSHOPIFY_TOKEN must be set (the access token clients must present)")
	}

	fake := shopifytest.NewServer(token, fixtures.Orders())
	mux := http.NewServeMux()
	mux.Handle("POST /admin/api/{version}/graphql.json", fake)
	mux.Handle("/_admin/", http.StripPrefix("/_admin", fake.AdminHandler()))

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("mock Shopify listening on http://%s/admin/api/2025-07/graphql.json", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
