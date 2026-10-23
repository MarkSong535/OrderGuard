# OrderGuard — Reliable E-Commerce Backend in Go

## 1. Project Overview

A production-oriented Go backend for e-commerce order management and customer-support integrations. The system emphasizes reliability, concurrency, security, fault tolerance and maintainability.

## 2. Objectives

| Objective | Status | Where |
|---|---|---|
| Demonstrate practical Go backend development | Done | Whole repository; standard library only |
| Build reliable integrations with external APIs | Done | `internal/provider/shopify`, `internal/resilience` |
| Handle high-concurrency workloads | Done | Request coalescing, bulkhead, load shedding, race-tested |
| Implement fault-tolerant request processing | Done | Timeouts, retries, circuit breaker, stale fallback |
| Follow maintainable software-engineering practices | Done | Layered packages, consumer-side interfaces, 88% coverage, CI |
| Support integration with AI-powered customer-service applications | Done | `support` role, provenance `meta`, no-fabrication guarantee, [API guidance](docs/API.md#notes-for-ai-customer-service-integrations) |

## 3. Technology Stack

| Area | Choice |
|---|---|
| Language | Go 1.25+ |
| HTTP | `net/http` (Go 1.22+ pattern routing) |
| API | REST/JSON |
| Database | Not used in the MVP (stateless; in-memory last-known-good cache) |
| External integration | Shopify Admin GraphQL API (`2025-07`) |
| Observability | `log/slog` JSON logs with request IDs; Prometheus text-format metrics |
| Testing | `testing`, `net/http/httptest`, race detector, benchmarks |
| Deployment | Multi-stage Dockerfile (distroless, non-root), docker compose, GitHub Actions CI |

## 4. System Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

### 4.1 Components

| Component | Package |
|---|---|
| HTTP/API layer | `internal/api` |
| Authentication and authorization | `internal/auth` |
| Order management service | `internal/orders` |
| External API provider abstraction | `internal/provider` (`shopify`, `mock`) |
| Retry and timeout handling | `internal/resilience`, `internal/orders` |
| Logging and monitoring | `internal/observability` |
| Data persistence layer | `internal/orders` stale cache (in memory); PostgreSQL deferred |

### 4.2 Request Lifecycle

Client → HTTP API → Authorization → Order Service → External Provider → Response Validation → Client

This is implemented as middleware (request ID, logging, recovery, headers, load shedding) → authentication → rate limit → handler → ID validation → coalescing → retry → bulkhead → breaker → provider → normalization and validation → per-order authorization → DTO.

### 4.3 Error Handling

There is a consistent, typed mapping for upstream failures, invalid requests, access violations and service unavailability. See [Architecture § Error handling](docs/ARCHITECTURE.md#error-handling) and [API § Error format](docs/API.md#error-format).

## 5. Core Features

### 5.1 Order Management

- [x] Retrieve order details: `GET /orders/{id}`
- [x] Retrieve fulfillment status: `GET /orders/{id}/fulfillment`
- [x] Search orders by regex over order numbers and item titles: `GET /orders?q=`, and MCP `search_orders`
- [x] Validate customer access: `auth.CanAccessOrder`, enforced on live and cached data
- [x] Normalize external API responses: `shopify.normalize` + `provider.Order.Validate`

### 5.2 Reliability

- [x] Request timeouts (per attempt and per request; server read/write/idle timeouts)
- [x] Context cancellation, propagated end to end; coalesced work is detached from any single caller
- [x] Bounded retries with exponential backoff and jitter
- [x] Circuit breaker (closed / open / half-open, generation-safe)
- [x] Graceful degradation: labelled stale fallback, `degraded` health, load shedding
- [x] Rate-limit handling: inbound per-key token bucket; outbound HTTP 429 `Retry-After` and GraphQL `THROTTLED` cost-based delay

### 5.3 Concurrency

- [x] Concurrent request processing, with identical in-flight lookups coalesced
- [x] Safe shared-state management (mutex-guarded components, immutable shared orders, race-tested)
- [x] Connection pooling (tuned `http.Transport`)
- [x] Resource limits (bulkhead, max in-flight, body size limits, header limits)

### 5.4 Security

- [x] Authentication (API keys stored as SHA-256 digests)
- [x] Order-level authorization (404 on denial, so IDs can't be enumerated)
- [x] Input validation (order IDs, request IDs, config)
- [x] Secrets management (env vars or `*_FILE`, no defaults, redaction)
- [x] Sensitive-data protection (masked email, no addresses, `no-store`, no secrets in logs or errors)

### 5.5 Observability

- [x] Structured logging (`slog` JSON)
- [x] Request IDs (propagated, validated, included in logs and error bodies)
- [x] Error metrics (by upstream outcome, HTTP status, auth failures, authz denials)
- [x] Latency measurements (HTTP and per-upstream-attempt histograms)
- [x] Health checks (`/health` and the container `HEALTHCHECK`)

## 6. API Design

The full reference is in [docs/API.md](docs/API.md).

- `GET /health`
- `GET /metrics`
- `GET /orders/{id}`
- `GET /orders/{id}/fulfillment`
- `GET /orders?q=<regex>`
- `POST /mcp` (Model Context Protocol tools for AI agents)

## 7. Testing Strategy

See [docs/TESTING.md](docs/TESTING.md).

| Type | Implementation |
|---|---|
| Unit | Every `internal/` package |
| Integration | `tests/`: full stack over loopback HTTP against `shopifytest` |
| Reliability | Timeouts, failures, retries, throttling, breaker open and recovery, stale fallback |
| Concurrency | Concurrent stress tests, all under `-race` |
| Performance | `tests/bench_test.go` (in-process and end-to-end) |

## 8. Repository Organization

```
orderguard/
├── cmd/
│   ├── server/
│   ├── mockshopify/
│   └── keytool/
├── internal/
│   ├── api/
│   ├── auth/
│   ├── config/
│   ├── orders/
│   ├── provider/  (shopify/, shopify/shopifytest/, mock/, fixtures/)
│   ├── resilience/
│   └── observability/
├── tests/
├── docs/
│   ├── API.md
│   ├── ARCHITECTURE.md
│   └── TESTING.md
├── scripts/demo.sh
├── .github/workflows/ci.yml
├── Dockerfile
├── docker-compose.yml
├── Makefile
├── go.mod
├── README.md
└── PROJECT.md
```

## 9. Development Roadmap

### Phase 1: MVP (done)

- [x] Implement HTTP server
- [x] Implement order service
- [x] Add mock provider
- [x] Implement authorization
- [x] Add unit tests

### Phase 2: Reliability (done)

- [x] Add context-aware timeouts
- [x] Implement retry policies
- [x] Add structured logging
- [x] Add integration tests

### Phase 3: Advanced Backend Features (done)

- [x] Add circuit breaker
- [x] Improve concurrency handling (coalescing, bulkhead, load shedding)
- [x] Add metrics
- [x] Integrate Shopify API (tested against a wire-compatible fake; not yet against a live store)

### Phase 4: Deployment (done)

- [x] Containerize application
- [x] Add CI pipeline
- [x] Run benchmarks
- [x] Document deployment

## 10. Quality Requirements

| Requirement | How it is met |
|---|---|
| Idiomatic Go | Standard library, small interfaces declared by consumers, `context` everywhere, `errors.Is`/`As`, table-driven tests, `gofmt`/`vet` in CI |
| Clear separation of responsibilities | api / orders / provider / resilience / auth / observability layering |
| Testable interfaces | `provider.Provider`, `api.OrderService`, injectable clocks |
| Consistent error handling | `provider.Kind` → `orders.Kind` → one HTTP status each; one JSON error envelope |
| No embedded credentials | Keys and tokens are generated at runtime or read from env vars/files; only digests are stored |
| No invented order information | Unknown enums → `unknown`; missing optional fields omitted; incomplete payloads → 502; stale data always labelled |
| Graceful handling of external service failures | Retries, breaker, stale fallback, `Retry-After`, `degraded` health |
| Documented API behavior | [docs/API.md](docs/API.md) |

## 11. Demonstration

`make demo` (`scripts/demo.sh`) reproduces:

1. Successful order retrieval
2. Unauthorized order access
3. Upstream API timeout
4. Retry behavior
5. Concurrent requests
6. Successful automated test execution

It also shows the circuit breaker, stale fallback and metrics.

The same scenarios are available interactively at https://order-demo.markso.ng: a React page with a store-connection control panel, the order database, and a support chat. The chat uses the canvas_ai agent, which reaches OrderGuard's search and lookup tools over MCP. Run it locally with `make demo-ui`.

## 12. Future Extensions

- Shopify webhooks (`orders/updated`, `fulfillments/create`), with HMAC verification, to invalidate or warm the cache
- Event-driven order updates
- AI chatbot integration: done for the demo (`internal/assistant`, a Go port of the canvas_ai agent using OrderGuard's MCP tools)
- Redis caching, plus distributed rate limits and breaker state
- Distributed tracing (OpenTelemetry; the request ID already propagates)
- Kubernetes deployment (Deployment + HPA on in-flight requests, PodDisruptionBudget)
- PostgreSQL persistence (audit log of support lookups)

## 13. Documentation

| Item | Location |
|---|---|
| Setup and installation instructions | [README.md](README.md#quick-start) |
| API reference | [docs/API.md](docs/API.md) |
| Architecture explanation | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) |
| Testing instructions | [docs/TESTING.md](docs/TESTING.md) |
| Performance results | [docs/TESTING.md § Performance](docs/TESTING.md#performance) |
| Known limitations | [README.md § Known limitations](README.md#known-limitations) |
