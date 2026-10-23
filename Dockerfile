# syntax=docker/dockerfile:1

# Interactive demo page (React), embedded into the server binary.
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/demo/web ./internal/demo/web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/server ./cmd/mockshopify

# Development-only fake Shopify endpoint (docker compose demo).
FROM gcr.io/distroless/static-debian12:nonroot AS mockshopify
COPY --from=build /out/mockshopify /mockshopify
USER nonroot:nonroot
EXPOSE 9090
ENTRYPOINT ["/mockshopify", "-addr", "0.0.0.0:9090"]

# Production image (default target). Distroless static: no shell, no package
# manager, unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/server /orderguard
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["/orderguard", "-healthcheck"]
ENTRYPOINT ["/orderguard"]
