# kpr (keeper)

Lightweight companion sidecar for an OCI distribution registry:
ephemeral images and lightweight retention cleanups, without the
weight of Harbor or Nexus. Inspired by ttl.sh.

> Starter state: the tree is still the skeleton the service will grow
> out of — project structure, docs, and toolchain are in place, the
> registry companion itself is not built yet. Start with
> [docs/goals.md](docs/goals.md).

## Features

- **Dual Server Architecture**
  - Management console on port 9300 (metrics, configuration, health)
  - Application server on port 8080 (SPA + API endpoints)
- **Embedded Assets**: Static files compiled into the binary
- **Clean Structure**: Separated concerns for management and application logic
- **Production Ready**: Dual-stack IPv4/IPv6, proper timeouts, logging
- **Zero Dependencies**: Pure Go stdlib for web serving
- **Cross-Platform**: Builds for Linux, macOS (Intel & ARM)

## Project Structure

```
kpr/
├── cmd/app/              # Application entry point
├── internal/
│   ├── web/              # Management console (port 9300)
│   │   ├── server.go     # HTTP server setup
│   │   ├── handlers.go   # Metrics, config, health endpoints
│   │   └── templates.go  # Dashboard HTML
│   └── app/              # Application server (port 8080)
│       ├── server.go     # HTTP server setup
│       ├── handlers.go   # API endpoints
│       ├── assets.go     # Static file embedding
│       └── static/       # SPA assets (HTML, CSS, JS)
├── Makefile              # Build automation
├── justfile              # Alternative build tool (just)
└── go.mod                # Go module definition
```

## Installation

### From Source

Requires Go 1.27+:

```bash
# Clone the repository
git clone https://nrtn.dev/catalyst/kpr.git
cd kpr

# Build the binary
make build
# or
just build

# Run locally
./kpr serve
```

### Build for Multiple Platforms

```bash
# Build for all platforms
make build-all

# Or specific platform
GOOS=linux GOARCH=amd64 go build -o kpr-linux-amd64 ./cmd/app
```

Supported platforms:
- `linux/amd64`, `linux/arm64`, `linux/arm`
- `darwin/amd64`, `darwin/arm64`

## Usage

### Start Both Servers

```bash
# Start management console (9300) and app server (8080)
kpr serve

# Custom ports
kpr serve --management-port 9300 --app-port 8080

# Custom hosts
kpr serve --management-host :: --app-host 0.0.0.0
```

### Environment Variables

```bash
# Management console
export KPR_MANAGEMENT_HOST=::     # Default: :: (dual-stack)
export KPR_MANAGEMENT_PORT=9300   # Default: 9300

# Application server
export KPR_APP_HOST=::            # Default: :: (dual-stack)
export KPR_APP_PORT=8080          # Default: 8080

kpr serve
```

## Endpoints

### Management Console (Port 9300)

- **Dashboard**: http://localhost:9300
  - Application metrics (uptime, requests, Go version)
  - Configuration viewer (version, commit, build time)
  - Environment variables

- **Metrics**: http://localhost:9300/metrics
  ```json
  {
    "uptime": "1h23m45s",
    "uptime_seconds": 5025,
    "requests": 42,
    "go_version": "go1.25.4",
    "version": "v1.0.0",
    "commit": "abc123",
    "build_time": "2025-12-08T09:30:00Z",
    "environment": { ... }
  }
  ```

- **Health**: http://localhost:9300/health
  - Returns 200 OK if service is healthy

### Application Server (Port 8080)

- **SPA**: http://localhost:8080
  - Single page application with embedded assets
  - Served from internal/app/static/

- **API Endpoints**:
  - `GET /api/hello` - Simple hello response
    ```json
    {
      "message": "Hello from kpr!",
      "timestamp": "2025-12-08T09:30:00Z",
      "version": "1.0.0"
    }
    ```

  - `GET /api/data` - Example data endpoint
    ```json
    {
      "items": ["Item 1", "Item 2", ...],
      "count": 5,
      "timestamp": "2025-12-08T09:30:00Z"
    }
    ```

## Dev stack

kpr runs beside stock `distribution` and `redis` via compose:

```bash
docker compose up --build
```

- kpr: `:8080` (app), `:9300` (management console)
- registry: `127.0.0.1:5000` (plain OCI distribution, `registry-config.yml`)
- redis: `127.0.0.1:6379` (TTL tracking and cleanup bookkeeping)

The registry's notification hook to kpr is stubbed out in
`registry-config.yml` until kpr implements the receiver.

## Development

### Building

```bash
# Build current platform
make build

# Build all platforms
make build-all

# Build with version info
VERSION=v1.0.0 make build
```

### Testing

```bash
# Run tests (gotestsum summary when installed, plain go test otherwise)
make test

# With coverage (terminal table + coverage.out + coverage.html)
make coverage

# Reprint the last coverage table without re-running tests
make coverage-report

# Linux-container suite, benchmarks, full gate
make test-linux
make bench
make check
```

Testing approach, coverage, and CI integration are documented in
[docs/TESTING.md](docs/TESTING.md); configuration in
[docs/CONFIG.md](docs/CONFIG.md).

### Code Quality

```bash
# Format code
make fmt

# Lint
make lint

# Run all checks
make check
```

### Running Locally

```bash
# Run with go run
make run

# Or directly
go run ./cmd/app serve
```

## OpenTelemetry (Optional)

kpr includes built-in OpenTelemetry support for distributed tracing, **disabled by default**.

### Enable OTEL

```bash
# Enable tracing
export OTEL_ENABLED=true
export OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4318

# Run with tracing
kpr serve
```

### Quick Setup with Jaeger

```bash
# Start Jaeger
docker run -d --name jaeger \
  -p 16686:16686 -p 4318:4318 \
  jaegertracing/all-in-one:latest

# Enable OTEL and run
export OTEL_ENABLED=true
kpr serve

# View traces at http://localhost:16686
```

### Configuration

- `OTEL_ENABLED` - Enable/disable (default: `false`)
- `OTEL_SERVICE_NAME` - Service name (default: `kpr`)
- `OTEL_EXPORTER_OTLP_ENDPOINT` - OTLP endpoint (default: `localhost:4318`)
- `OTEL_ENVIRONMENT` - Environment (default: `development`)
- `OTEL_SERVICE_VERSION` - Version (default: `dev`)

See `internal/otel/README.md` for detailed configuration and cloud provider setup.

## Customization

### Adding New API Endpoints

Edit `internal/app/handlers.go`:

```go
func YourHandler(w http.ResponseWriter, r *http.Request) {
    // Your logic here
    json.NewEncoder(w).Encode(response)
}
```

Register in `internal/app/server.go`:

```go
mux.HandleFunc("/api/your-endpoint", YourHandler)
```

### Modifying the SPA

Static assets are in `internal/app/static/`:
- `index.html` - Main HTML
- `style.css` - Styling
- `app.js` - JavaScript

Assets are embedded at build time using `go:embed`.

### Adding Management Console Metrics

Edit `internal/web/handlers.go` to add custom metrics:

```go
type metricsData struct {
    // Add your custom fields
    CustomMetric string `json:"custom_metric"`
}
```

## Deployment

### Systemd Service

See `contrib/systemd/` for systemd service files and installation instructions.

```bash
# Quick install
sudo cp contrib/systemd/kpr.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now kpr
```

### Docker

kpr uses a two-stage build approach: compile binaries with make/just (fast, cached) and package with Docker (simple, consistent).

#### Quick Start

```bash
# Build the image for current platform
make docker-build
# or
just docker-build

# Run the container
make docker-run
```

#### Build Configuration

Docker builds:
- Automatically tag as `latest` for version tags (e.g., `v1.0.0`)
- Use dev version tag for non-release builds
- Support multiplatform builds (amd64, arm64, armv7)
- Package pre-built binaries from `dist/` directory

#### Build from Source

```bash
# Build for current platform (builds binary + packages in Docker)
make docker-build

# Build with custom version
VERSION=v1.0.0 make docker-build

# Build binary only (outputs to dist/)
make build
```

#### Multiplatform Build

Build for multiple architectures (requires Docker Buildx):

```bash
# Build and push multiplatform image (builds all binaries first)
make docker-build-multiplatform
```

Supported platforms:
- `linux/amd64` (x86_64)
- `linux/arm64` (ARM64/AArch64)
- `linux/arm/v7` (ARMv7)

#### Run with Environment Variables

```bash
docker run --rm -p 8080:8080 -p 9300:9300 \
  -e OTEL_ENABLED=true \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=tempo:4318 \
  -e KPR_MANAGEMENT_HOST=0.0.0.0 \
  -e KPR_APP_HOST=0.0.0.0 \
  nrtn.dev/catalyst/kpr:latest
```

#### Docker Compose

```yaml
version: '3.8'
services:
  kpr:
    image: nrtn.dev/catalyst/kpr:latest
    ports:
      - "8080:8080"
      - "9300:9300"
    environment:
      - OTEL_ENABLED=false
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://localhost:9300/health"]
      interval: 30s
      timeout: 3s
      retries: 3
```

#### Image Details

- **Base Image**: Alpine Linux (minimal footprint)
- **Size**: ~15-20 MB (compressed)
- **User**: Runs as non-root user `kpr` (UID 1000)
- **Security**: No capabilities, read-only filesystem compatible
- **Health Check**: Built-in health check on port 9300

## License

[Your License Here]

## Contributing

Contributions welcome! This is a blueprint/skeleton project designed to be forked and customized for your needs.

## Architecture Notes

### Why Two Servers?

- **Separation of Concerns**: Management operations separate from application logic
- **Security**: Management console can be firewalled separately
- **Different Audiences**: Ops teams use management console, users use application
- **Independent Scaling**: Can run management console on a single instance

### Embedded Assets

Static files are compiled into the binary using Go's `embed` package. This means:
- Single binary deployment
- No external file dependencies
- Faster startup (no disk I/O for assets)
- Easier containerization

### Future Enhancements

This blueprint can be extended with:
- Database connection management (PostgreSQL, MySQL, etc.)
- Authentication/authorization middleware (JWT, OAuth)
- Prometheus metrics integration
- ✅ **OpenTelemetry tracing** (included, disabled by default)
- gRPC API endpoints
- WebSocket support
- GraphQL API
- Rate limiting and circuit breakers
