# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.27-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make

WORKDIR /build

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build arguments for version info
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s \
    -X 'nrtn.dev/catalyst/kpr/internal/config.Version=${VERSION}' \
    -X 'nrtn.dev/catalyst/kpr/internal/config.Commit=${COMMIT}' \
    -X 'nrtn.dev/catalyst/kpr/internal/config.BuildTime=${BUILD_TIME}'" \
    -o kpr ./cmd/app

# Runtime stage
FROM alpine:latest

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1000 kpr && \
    adduser -D -u 1000 -G kpr kpr

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/kpr /usr/local/bin/kpr

# Set ownership
RUN chown -R kpr:kpr /app

# Switch to non-root user
USER kpr

# Expose ports
EXPOSE 8080 9300

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:9300/health || exit 1

# Default command
CMD ["kpr", "serve"]
