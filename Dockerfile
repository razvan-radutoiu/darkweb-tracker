FROM golang:1.22-alpine AS builder

WORKDIR /build

# Install git (needed for go mod download with private deps, and for version info)
RUN apk add --no-cache git

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
    -o tracker \
    ./cmd/tracker

# ── Runtime image (minimal) ───────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /build/tracker .

# Default config (users mount their own via -v)
COPY config.yaml .

# Persist database and reports
VOLUME ["/app/data_leaks.db", "/app/archive", "/app/rss"]

ENTRYPOINT ["./tracker"]
CMD ["--config", "config.yaml", "--db", "data_leaks.db"]
