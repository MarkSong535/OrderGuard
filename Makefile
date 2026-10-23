GO      ?= go
BIN     := bin
PKGS    := ./...
KEYS    ?= keys.json

.PHONY: all build test race cover bench lint fmt vet run run-shopify mockshopify keys demo demo-ui web deploy-oci docker compose-up clean

all: lint test build

build: ## Build all binaries into ./bin
	$(GO) build -trimpath -o $(BIN)/ ./cmd/...

test: ## Unit + integration tests
	$(GO) test -count=1 $(PKGS)

race: ## Tests with the race detector
	$(GO) test -race -count=1 $(PKGS)

cover: ## Coverage across all internal packages
	$(GO) test -race -count=1 -coverpkg=./internal/... -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

bench: ## Benchmarks
	$(GO) test -run '^$$' -bench . -benchmem -benchtime 3s ./tests/ ./internal/...

fmt:
	gofmt -w .

vet:
	$(GO) vet $(PKGS)

lint: vet ## gofmt check + go vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

keys: ## Create $(KEYS) with demo keys for alice, bob and a support bot (prints plaintext keys once)
	@if [ -f $(KEYS) ]; then echo "$(KEYS) already exists; delete it to regenerate"; exit 1; fi
	@echo "alice (customer 7001): $$($(GO) run ./cmd/keytool -keys-file $(KEYS) -id alice -role customer -customer-id 7001)"
	@echo "bob   (customer 7002): $$($(GO) run ./cmd/keytool -keys-file $(KEYS) -id bob -role customer -customer-id 7002)"
	@echo "support-bot          : $$($(GO) run ./cmd/keytool -keys-file $(KEYS) -id support-bot -role support)"

run: ## Run with the in-memory provider (requires $(KEYS))
	ORDERGUARD_API_KEYS_FILE=$(KEYS) ORDERGUARD_LOG_FORMAT=text $(GO) run ./cmd/server

mockshopify: ## Run the fake Shopify server on :9090 (requires MOCKSHOPIFY_TOKEN)
	$(GO) run ./cmd/mockshopify

run-shopify: ## Run against the fake Shopify server (requires MOCKSHOPIFY_TOKEN and $(KEYS))
	ORDERGUARD_API_KEYS_FILE=$(KEYS) ORDERGUARD_PROVIDER=shopify ORDERGUARD_LOG_FORMAT=text \
	SHOPIFY_ENDPOINT=http://127.0.0.1:9090/admin/api/2025-07/graphql.json \
	SHOPIFY_ALLOW_INSECURE_ENDPOINT=true SHOPIFY_ACCESS_TOKEN=$$MOCKSHOPIFY_TOKEN \
	$(GO) run ./cmd/server

demo: ## Scripted end-to-end demonstration of all reliability scenarios
	./scripts/demo.sh

demo-ui: ## Interactive demo on http://127.0.0.1:8095 (set OPENAI_API_KEY to enable the AI assistant)
	ORDERGUARD_DEMO=true ORDERGUARD_ADDR=127.0.0.1:8095 ORDERGUARD_LOG_FORMAT=text $(GO) run ./cmd/server

web: ## Rebuild the demo page (React) into internal/demo/web
	cd web && npm ci --no-audit --no-fund && npm run build

deploy-oci: ## Deploy the interactive demo to the OCI box
	./deploy/oci/deploy.sh

docker: ## Build the production image
	docker build --target server -t orderguard:latest .

compose-up: ## Run OrderGuard + fake Shopify in Docker
	@[ -f $(KEYS) ] || $(MAKE) keys
	MOCKSHOPIFY_TOKEN=$${MOCKSHOPIFY_TOKEN:-$$(openssl rand -hex 24)} ORDERGUARD_API_KEYS="$$(cat $(KEYS))" docker compose up --build

clean:
	rm -rf $(BIN) coverage.out
