#!/usr/bin/env bash
# Reproducible end-to-end demonstration of OrderGuard.
#
# Builds the binaries, generates throwaway credentials, starts the fake
# Shopify server and OrderGuard (shopify provider) on loopback, then walks
# through each scenario with curl. Everything is torn down on exit.
#
# Usage: ./scripts/demo.sh [--skip-tests]
set -euo pipefail

cd "$(dirname "$0")/.."
SKIP_TESTS=${1:-}

WORK=$(mktemp -d)
OG_PORT=${OG_PORT:-18080}
SHOP_PORT=${SHOP_PORT:-19090}
OG="http://127.0.0.1:${OG_PORT}"
SHOP="http://127.0.0.1:${SHOP_PORT}"

cleanup() {
  [[ -n "${OG_PID:-}" ]] && kill "$OG_PID" 2>/dev/null || true
  [[ -n "${SHOP_PID:-}" ]] && kill "$SHOP_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
step() { printf '\n\033[1;34m== %s ==\033[0m\n' "$*"; }
pretty() { if command -v jq >/dev/null; then jq -C . 2>/dev/null || cat; else cat; echo; fi; }

# call KEY PATH -> prints status line, selected headers and body
call() {
  local key=$1 path=$2 hdr=() ms
  [[ -n "$key" ]] && hdr=(-H "Authorization: Bearer $key")
  ms=$(curl -sS -D "$WORK/h" -o "$WORK/b" -w '%{time_total}' "${hdr[@]+"${hdr[@]}"}" "$OG$path" | awk '{printf "%d", $1 * 1000}')
  printf '$ curl %s%s\n' "$path" "$([[ -n "$key" ]] && echo ' -H "Authorization: Bearer <key>"')"
  printf '%s  (%s ms)\n' "$(head -1 "$WORK/h" | tr -d '\r')" "$ms"
  grep -iE '^(retry-after|warning|x-request-id):' "$WORK/h" | tr -d '\r' || true
  pretty < "$WORK/b"
}

faults() { curl -sS -X PUT "$SHOP/_admin/faults" -d "$1" >/dev/null; echo "[fake shopify faults] $1"; }
upstream_count() { curl -sS "$SHOP/_admin/stats" | sed -E 's/.*:([0-9]+).*/\1/'; }
reset_count() { curl -sS -X POST "$SHOP/_admin/stats/reset"; }

step "Setup"
go build -o "$WORK/" ./cmd/server ./cmd/mockshopify ./cmd/keytool
KEYS="$WORK/keys.json"
ALICE=$("$WORK/keytool" -keys-file "$KEYS" -id alice -role customer -customer-id 7001)
BOB=$("$WORK/keytool" -keys-file "$KEYS" -id bob -role customer -customer-id 7002)
SUPPORT=$("$WORK/keytool" -keys-file "$KEYS" -id support-bot -role support)
SHOP_TOKEN=$(openssl rand -hex 24)
echo "Generated throwaway API keys for alice (customer 7001), bob (customer 7002) and support-bot."
echo "Key table (digests only, no plaintext keys):"; pretty < "$KEYS"

MOCKSHOPIFY_TOKEN=$SHOP_TOKEN "$WORK/mockshopify" -addr "127.0.0.1:${SHOP_PORT}" >"$WORK/shop.log" 2>&1 &
SHOP_PID=$!

ORDERGUARD_ADDR="127.0.0.1:${OG_PORT}" \
ORDERGUARD_PROVIDER=shopify \
ORDERGUARD_API_KEYS_FILE="$KEYS" \
SHOPIFY_ENDPOINT="$SHOP/admin/api/2025-07/graphql.json" \
SHOPIFY_ALLOW_INSECURE_ENDPOINT=true \
SHOPIFY_ACCESS_TOKEN=$SHOP_TOKEN \
ORDERGUARD_UPSTREAM_ATTEMPT_TIMEOUT=500ms \
ORDERGUARD_UPSTREAM_REQUEST_TIMEOUT=2s \
ORDERGUARD_RETRY_BASE_DELAY=100ms \
ORDERGUARD_BREAKER_FAILURE_THRESHOLD=8 \
ORDERGUARD_BREAKER_OPEN_TIMEOUT=3s \
ORDERGUARD_RATE_LIMIT_RPS=500 ORDERGUARD_RATE_LIMIT_BURST=500 \
"$WORK/server" >"$WORK/server.log" 2>&1 &
OG_PID=$!

for _ in $(seq 50); do curl -fsS "$OG/health" >/dev/null 2>&1 && break; sleep 0.1; done
call "" /health

step "1. Successful order retrieval"
call "$ALICE" /orders/1001
call "$ALICE" /orders/1001/fulfillment

step "2. Unauthorized order access"
bold "No API key:"
call "" /orders/1001
bold "Bob requests Alice's order (404, indistinguishable from a missing order):"
call "$BOB" /orders/1001
bold "Malformed order ID is rejected before any upstream call:"
call "$ALICE" "/orders/1001%20OR%201=1"
echo; echo "Server log entry for the denial:"
grep 'order access denied' "$WORK/server.log" | tail -1

step "3. Upstream API timeout"
faults '{"latency_ms":3000}'
reset_count
call "$ALICE" /orders/1003
echo "Upstream attempts: $(upstream_count) (each cut off at the 500ms attempt timeout; total budget 2s)"
faults '{}'

step "4. Retry behaviour"
faults '{"fail_first":2,"fail_status":503}'
reset_count
call "$ALICE" /orders/1003
echo "Upstream attempts: $(upstream_count) (two 503s retried with exponential backoff, third succeeded)"
echo "Retry log entries:"
grep 'retrying upstream call' "$WORK/server.log" | tail -2
bold "Shopify GraphQL throttling (THROTTLED) is retried after the bucket-refill delay:"
faults '{"throttle":true}'
( sleep 0.4; curl -sS -X PUT "$SHOP/_admin/faults" -d '{}' >/dev/null ) &
CLEAR_PID=$!
reset_count
call "$BOB" /orders/1002 | head -2
wait "$CLEAR_PID"
echo "Upstream attempts: $(upstream_count) (first throttled; retry waited ~1s for the cost bucket to refill)"

step "5. Concurrent requests"
faults '{"latency_ms":100}'
reset_count
N=300
echo "Firing $N requests, 50 at a time, from three principals across four orders..."
for i in $(seq $N); do
  case $((i % 6)) in
    0) echo "$ALICE 1001";; 1) echo "$ALICE 1003";; 2) echo "$BOB 1002";;
    3) echo "$BOB 1001";; 4) echo "$SUPPORT 1004";; 5) echo "$SUPPORT 1002";;
  esac
done | xargs -P 50 -L 1 sh -c 'curl -s -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $0" '"$OG"'/orders/$1' \
  | sort | uniq -c | awk '{printf "  HTTP %s: %s responses\n", $2, $1}'
echo "Expected: 250 × 200 and 50 × 404 (bob → alice's order)."
echo "Upstream GraphQL calls: $(upstream_count) for $N client requests (in-flight lookups are coalesced)"
faults '{}'

step "Bonus: circuit breaker and graceful degradation"
faults '{"fail_rate":1}'
for _ in 1 2 3; do curl -s -o /dev/null -H "Authorization: Bearer $SUPPORT" "$OG/orders/1004"; done
bold "Breaker is now open; an uncached order fails fast without touching Shopify:"
reset_count
call "$SUPPORT" /orders/2001
echo "Upstream attempts: $(upstream_count)"
call "" /health
bold "Previously fetched orders are still served, labelled stale:"
call "$ALICE" /orders/1001
faults '{}'
echo "Waiting for the breaker cool-down (3s)..."; sleep 3.2
call "$SUPPORT" /orders/1004 | head -2
bold "Selected metrics:"
curl -sS "$OG/metrics" | grep -E '^orderguard_(upstream_requests_total|upstream_retries_total|circuit_breaker_transitions_total|stale_responses_total|deduplicated_requests_total|authz_denied_total)'

step "6. Automated test suite"
if [[ "$SKIP_TESTS" == "--skip-tests" ]]; then
  echo "skipped (--skip-tests)"
else
  go test -race -count=1 ./...
fi

bold "Demo complete."
