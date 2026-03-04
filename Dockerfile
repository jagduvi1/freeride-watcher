# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

# Install git (needed by go module downloads for some deps)
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# Cache module downloads separately from source code.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -trimpath \
    -o server \
    ./cmd/server

# ── Stage 2: Run ──────────────────────────────────────────────────────────────
# Alpine gives us ca-certs, tzdata, and wget for the healthcheck while staying tiny.
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app
COPY --from=builder /build/server .

# Templates and static files are embedded inside the binary.
VOLUME ["/data"]

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/server"]
