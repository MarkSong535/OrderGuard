# OrderGuard

A Go backend for looking up e-commerce orders reliably. 

## Interactive demo

A sample demo build with React: [https://order-demo.markso.ng](https://order-demo.markso.ng)

- **Simulate as**: Alice, Bob, a support agent, or a guest.
- **Store connection**: Simulate connection conditions to the upstream store (latency, failure rate, throttling).
- **Store orders**: every order in the demo store, with the ones the chosen person may see highlighted. This is available to the support agent and the backend service, not the customer. 
- **Support chat**: An agent with order details provided by an MCP server. Prevented unauthorized access with more control.

Run it locally:

```bash
make demo-ui     # http://127.0.0.1:8095; export OPENAI_API_KEY first to enable the AI assistant
```

## Quick start

Requires Go 1.25+.

```bash
make keys        # writes keys.json (SHA-256 digests only) and prints three demo API keys once
make run         # starts on :8080 with the in-memory fixture provider
```

```bash
curl -s -H "Authorization: Bearer $ALICE_KEY" localhost:8080/orders/1001 | jq
```

Fixture orders: `1001` and `1003` belong to customer 7001 (alice), `1002` belongs to 7002 (bob), and `1004` is a guest checkout. All fixture data is synthetic and uses `example.com` addresses.

### Scripted demonstration

```bash
make demo
```

This builds everything, generates throwaway credentials, and starts the fake Shopify server plus OrderGuard on loopback. It then demonstrates each scenario and runs the test suite:

1. Successful order retrieval
2. Unauthorized access
3. Upstream timeout
4. Retry behaviour
5. Concurrent requests

### Against the simulated Shopify server manually

```bash
export MOCKSHOPIFY_TOKEN=$(openssl rand -hex 24)
make mockshopify &          # :9090
make run-shopify            # OrderGuard on :8080 using the Shopify provider
curl -X PUT localhost:9090/_admin/faults -d '{"fail_first":2}'   # inject faults
```

### Docker

```bash
make docker                 # distroless, non-root, ~9 MB image with a HEALTHCHECK
make compose-up             # OrderGuard + fake Shopify via docker compose
```

### Real Shopify

Create a custom app with the `read_orders` and `read_customers` Admin API scopes. Then:

```bash
export ORDERGUARD_PROVIDER=shopify
export SHOPIFY_SHOP_DOMAIN=your-shop.myshopify.com
export SHOPIFY_ACCESS_TOKEN_FILE=/run/secrets/shopify_token   # or SHOPIFY_ACCESS_TOKEN
export ORDERGUARD_API_KEYS_FILE=/run/secrets/orderguard_keys.json
./bin/server
```

## API

| Endpoint | Auth | Description |
|---|---|---|
| `GET /health` | none | Health of the service (`ok` / `degraded`) |
| `GET /metrics` | none (restrict at the network layer) | Prometheus metrics |
| `GET /orders?q=<regex>` | Bearer key | Search the caller's orders: case-insensitive regex over order numbers and item titles |
| `GET /orders/{id}` | Bearer key | Order details, normalized |
| `GET /orders/{id}/fulfillment` | Bearer key | Fulfillment status and tracking |
| `POST /mcp` | Bearer key | MCP server with `search_orders`, `get_order` and `get_order_tracking` tools for AI agents |

The full schemas, status semantics and guidance for AI integrations are in **[docs/API.md](docs/API.md)**.

## API keys

```bash
go run ./cmd/keytool -keys-file keys.json -id support-bot -role support
go run ./cmd/keytool -keys-file keys.json -id cust-7001 -role customer -customer-id 7001
```

The plaintext key is printed once. The file stores only `{"id","key_sha256","role","customer_id"}` and is created with mode 0600. Roles:

- `customer`: only orders owned by its `customer_id`.
- `support`: any order.

## Configuration

All settings come from environment variables. Each secret can instead be read from a file through the `*_FILE` variant.

| Variable | Default | Description |
|---|---|---|
| `ORDERGUARD_ADDR` | `:8080` | Listen address |
| `ORDERGUARD_LOG_LEVEL` / `_FORMAT` | `info` / `json` | `debug`/`info`/`warn`/`error`; `json`/`text` |
| `ORDERGUARD_API_KEYS_FILE` / `ORDERGUARD_API_KEYS` | — (required) | Key table: a file path, or the JSON inline |
| `ORDERGUARD_PROVIDER` | `mock` | `mock` or `shopify` |
| `SHOPIFY_SHOP_DOMAIN` | — | `<shop>.myshopify.com` |
| `SHOPIFY_API_VERSION` | `2025-07` | Admin API version |
| `SHOPIFY_ACCESS_TOKEN` / `_FILE` | — | Admin API token (required for `shopify`) |
| `SHOPIFY_ENDPOINT` | derived | Override the GraphQL URL (for the fake server) |
| `SHOPIFY_ALLOW_INSECURE_ENDPOINT` | `false` | Allow `http://` overrides (local development only) |
| `ORDERGUARD_UPSTREAM_ATTEMPT_TIMEOUT` | `2s` | Deadline per upstream call |
| `ORDERGUARD_UPSTREAM_REQUEST_TIMEOUT` | `5s` | Deadline across all attempts |
| `ORDERGUARD_RETRY_MAX_ATTEMPTS` | `3` | Total attempts (1–10) |
| `ORDERGUARD_RETRY_BASE_DELAY` / `_MAX_DELAY` | `100ms` / `1s` | Backoff bounds |
| `ORDERGUARD_BREAKER_FAILURE_THRESHOLD` | `5` | Consecutive transient failures before the breaker opens |
| `ORDERGUARD_BREAKER_OPEN_TIMEOUT` | `10s` | Cool-down before a probe |
| `ORDERGUARD_UPSTREAM_MAX_CONCURRENCY` | `32` | Bulkhead size |
| `ORDERGUARD_UPSTREAM_QUEUE_TIMEOUT` | `250ms` | Max wait for a bulkhead slot |
| `ORDERGUARD_STALE_CACHE_TTL` / `_SIZE` | `10m` / `1000` | Last-known-good fallback (`0` disables) |
| `ORDERGUARD_RATE_LIMIT_RPS` / `_BURST` | `10` / `20` | Per-key token bucket (`0` disables) |
| `ORDERGUARD_MAX_INFLIGHT` | `512` | Load-shedding threshold |
| `ORDERGUARD_SHUTDOWN_TIMEOUT` | `15s` | Graceful drain on SIGTERM |
| `ORDERGUARD_MOCK_LATENCY` / `_FAILURE_RATE` | `0` / `0` | Fault injection for the `mock` provider |
| `ORDERGUARD_DEMO` | `false` | Interactive demo mode (synthetic data only; uses demo-tuned timeouts) |
| `OPENAI_API_KEY` / `_FILE` | — | Enables the demo's AI assistant |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Chat Completions endpoint (any compatible API) |
| `ORDERGUARD_ASSISTANT_MODEL` / `_REASONING_EFFORT` | `gpt-6-luna` / `none` | Assistant model settings (canvas_ai's defaults) |
| `ORDERGUARD_DEMO_CHAT_PER_MINUTE` / `_DAILY_CAP` | `6` / `300` | Chat limits per visitor and per day |
| `ORDERGUARD_DEMO_TRUST_CF_CONNECTING_IP` | `false` | Identify visitors by `CF-Connecting-IP` (only behind Cloudflare) |