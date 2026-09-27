# kpr development tasks

VERSION := `git describe --tags --always --dirty 2>/dev/null || echo "dev"`
COMMIT := `git rev-parse --short HEAD 2>/dev/null || echo "unknown"`
BUILD_TIME := `date -u +%Y-%m-%dT%H:%M:%SZ`
LDFLAGS := "-s -w -X nrtn.dev/catalyst/kpr/internal/web.Version=" + VERSION + " -X nrtn.dev/catalyst/kpr/internal/web.Commit=" + COMMIT + " -X nrtn.dev/catalyst/kpr/internal/web.BuildTime=" + BUILD_TIME

# Default recipe - show available commands
default:
    @just --list

# Show help message
help:
    @just --list

# Build for all platforms
all: build-all

# Check build prerequisites
prereqs:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Checking build prerequisites..."
    echo ""

    # Required tools (for building)
    REQUIRED=(go git)
    # Optional tools (for development)
    OPTIONAL=(golangci-lint goreleaser upx)

    all_good=true

    echo "Required tools:"
    for tool in "${REQUIRED[@]}"; do
        if command -v "$tool" &> /dev/null; then
            version=$($tool version 2>&1 | head -n1 || echo "installed")
            echo "  ✓ $tool - $version"
        else
            echo "  ✗ $tool - NOT FOUND"
            all_good=false
        fi
    done

    echo ""
    echo "Optional tools:"
    for tool in "${OPTIONAL[@]}"; do
        if command -v "$tool" &> /dev/null; then
            version=$($tool version 2>&1 | head -n1 || echo "installed")
            echo "  ✓ $tool - $version"
        else
            echo "  - $tool - not installed (optional)"
        fi
    done

    echo ""
    if [ "$all_good" = true ]; then
        echo "✓ All required prerequisites are installed!"
        echo ""
        echo "To install optional tools:"
        echo "  golangci-lint: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"
        echo "  goreleaser:    brew install goreleaser"
        echo "  upx:           brew install upx  # or: apt-get install upx"
    else
        echo "✗ Missing required tools. Please install them first."
        exit 1
    fi

# Build the binary for current platform
build:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p dist
    GOOS=$(go env GOOS)
    GOARCH=$(go env GOARCH)
    CGO_ENABLED=0 go build -ldflags "{{LDFLAGS}}" -o dist/kpr-$GOOS-$GOARCH ./cmd/app
    echo "Built: dist/kpr-$GOOS-$GOARCH"

# Build all platform binaries
build-all: build-linux-amd64 build-linux-arm64 build-linux-arm build-darwin-amd64 build-darwin-arm64

# Build for Linux amd64
build-linux-amd64:
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "{{LDFLAGS}}" -o dist/kpr-linux-amd64 ./cmd/app

# Build for Linux arm64
build-linux-arm64:
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "{{LDFLAGS}}" -o dist/kpr-linux-arm64 ./cmd/app

# Build for Linux arm (ARMv7)
build-linux-arm:
    CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "{{LDFLAGS}}" -o dist/kpr-linux-arm ./cmd/app

# Build for macOS amd64 (Intel)
build-darwin-amd64:
    GOOS=darwin GOARCH=amd64 go build -ldflags "{{LDFLAGS}}" -o dist/kpr-darwin-amd64 ./cmd/app

# Build for macOS arm64 (Apple Silicon)
build-darwin-arm64:
    GOOS=darwin GOARCH=arm64 go build -ldflags "{{LDFLAGS}}" -o dist/kpr-darwin-arm64 ./cmd/app

# Build release archives for all platforms
release: clean-dist
    @echo "Building release {{VERSION}}..."
    @mkdir -p dist
    just build-all
    cd dist && sha256sum kpr-* > checksums.txt
    @echo "Release built: dist/"
    @ls -lh dist/

# Run the servers locally
run:
    go run ./cmd/app serve

# Format code
fmt:
    go fmt ./...

# Check code formatting
fmt-check:
    test -z "$(gofmt -l .)"

# Lint code (uses golangci-lint if available, otherwise go vet)
lint:
    #!/usr/bin/env bash
    if command -v golangci-lint &> /dev/null; then
        echo "Running golangci-lint..."
        golangci-lint run
    else
        echo "golangci-lint not found, using go vet..."
        echo "Install golangci-lint for better linting:"
        echo "  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"
        echo ""
        go vet ./...
    fi

# Run tests
test:
    go test -v -race ./...

# Run tests with coverage (coverprofile + terminal summary + HTML report)
coverage:
    go test -race -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | tail -n 20
    go tool cover -html=coverage.out -o coverage.html
    @echo "Coverage report: coverage.html"

# Reprint the coverage summary from the last run without re-running tests
coverage-report:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ ! -f coverage.out ]; then
        echo "No coverage.out found — run 'just coverage' first."
        exit 1
    fi
    go tool cover -func=coverage.out | tail -n 20

# Run benchmarks (no tests, measurements only)
bench:
    go test -run=NONE -bench=. -benchmem ./...

# Test Docker image
test-docker:
    #!/usr/bin/env bash
    set -euo pipefail
    VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
    HOST="[::1]"

    echo "Cleaning up any existing container..."
    docker rm -f kpr-test 2>/dev/null || true

    echo "Starting container..."
    docker run -d --name kpr-test \
        -p 8080:8080 -p 9300:9300 \
        {{DOCKER_IMAGE}}:${VERSION}

    echo ""
    echo "Waiting for container to start..."
    sleep 5

    echo ""
    echo "Container status:"
    docker ps -a | grep kpr-test || echo "Container not found!"

    echo ""
    echo "Container inspect:"
    docker inspect --format='{{{{.State.Status}}}} - ExitCode={{{{.State.ExitCode}}}} {{{{if .State.Error}}}}- Error={{{{.State.Error}}}}{{{{end}}}}' kpr-test || echo "Failed to inspect"

    echo ""
    echo "Container logs:"
    docker logs kpr-test 2>&1 || echo "Failed to get logs"

    echo ""
    echo "Testing endpoints from inside container..."
    HEALTH_OK=false
    APP_OK=false

    echo ""
    echo "Testing health endpoint (IPv6 [::1]:9300)..."
    if docker exec kpr-test wget -q -O- http://[::1]:9300/health >/dev/null 2>&1; then
        echo "✓ IPv6 health check passed"
        HEALTH_OK=true
    else
        echo "✗ IPv6 health check failed"
    fi

    echo "Testing health endpoint (IPv4 127.0.0.1:9300)..."
    if docker exec kpr-test wget -q -O- http://127.0.0.1:9300/health >/dev/null 2>&1; then
        echo "✓ IPv4 health check passed"
        HEALTH_OK=true
    else
        echo "✗ IPv4 health check failed"
    fi

    echo ""
    echo "Testing app endpoint (IPv6 [::1]:8080)..."
    if docker exec kpr-test wget -q -O- http://[::1]:8080/ >/dev/null 2>&1; then
        echo "✓ IPv6 app endpoint passed"
        APP_OK=true
    else
        echo "✗ IPv6 app endpoint failed"
    fi

    echo "Testing app endpoint (IPv4 127.0.0.1:8080)..."
    if docker exec kpr-test wget -q -O- http://127.0.0.1:8080/ >/dev/null 2>&1; then
        echo "✓ IPv4 app endpoint passed"
        APP_OK=true
    else
        echo "✗ IPv4 app endpoint failed"
    fi

    echo ""
    echo "Final container logs:"
    docker logs kpr-test 2>&1 || echo "Failed to get logs"

    echo ""
    echo "Stopping and removing container..."
    docker stop kpr-test 2>/dev/null || echo "Container already stopped"
    docker rm kpr-test 2>/dev/null || echo "Failed to remove container"

    if [ "$HEALTH_OK" = false ] || [ "$APP_OK" = false ]; then
        echo ""
        echo "Tests failed!"
        exit 1
    fi

    echo ""
    echo "All tests passed!"

# Run all checks
check: fmt-check lint

# Fix all auto-fixable issues
fix: fmt

# Clean build artifacts
clean:
    rm -f coverage.out coverage.html
    go clean

# Clean dist directory
clean-dist:
    rm -rf dist/

# Install dependencies
deps:
    go mod download
    go mod tidy

# Show version information
version:
    @echo "Version:    {{VERSION}}"
    @echo "Commit:     {{COMMIT}}"
    @echo "Build Time: {{BUILD_TIME}}"
    @echo ""
    @go version

# Show module info
info:
    @echo "Version:    {{VERSION}}"
    @echo "Commit:     {{COMMIT}}"
    @echo "Build Time: {{BUILD_TIME}}"
    @echo ""
    @go version
    @echo ""
    @go list -m all

# Install binary to /usr/local/bin
install: build
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Installing kpr to /usr/local/bin..."
    GOOS=$(go env GOOS)
    GOARCH=$(go env GOARCH)
    sudo cp dist/kpr-$GOOS-$GOARCH /usr/local/bin/kpr
    echo "Installed successfully"

# Remove binary from /usr/local/bin
uninstall:
    @echo "Removing kpr from /usr/local/bin..."
    sudo rm -f /usr/local/bin/kpr
    @echo "Uninstalled successfully"

# Docker configuration
DOCKER_REGISTRY := env_var_or_default("DOCKER_REGISTRY", "nrtn.dev/catalyst")
DOCKER_IMAGE := DOCKER_REGISTRY + "/kpr"

# Build Docker image for current platform
docker-build:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Building binary for current platform..."
    mkdir -p dist
    GOARCH=$(go env GOARCH)
    CGO_ENABLED=0 GOOS=linux go build -ldflags "{{LDFLAGS}}" -o dist/kpr-linux-$GOARCH ./cmd/app
    echo "Packaging Docker image {{DOCKER_IMAGE}}:{{VERSION}}..."

    TAGS="-t {{DOCKER_IMAGE}}:{{VERSION}}"
    if echo "{{VERSION}}" | grep -qE '^v?[0-9]+\.[0-9]+\.[0-9]+$'; then
        TAGS="$TAGS -t {{DOCKER_IMAGE}}:latest"
    fi

    docker buildx build \
        -f Dockerfile.package \
        $TAGS \
        --load .

# Build and push multiplatform Docker images
docker-build-multiplatform: build-all
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Packaging multiplatform Docker images {{DOCKER_IMAGE}}:{{VERSION}}..."

    # Build all platforms at once (Dockerfile auto-picks binary based on platform)
    TAGS="-t {{DOCKER_IMAGE}}:{{VERSION}}"
    if echo "{{VERSION}}" | grep -qE '^v?[0-9]+\.[0-9]+\.[0-9]+$'; then
        TAGS="$TAGS -t {{DOCKER_IMAGE}}:latest"
    fi

    docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 \
        -f Dockerfile.package \
        $TAGS \
        --push .

    echo "Multiplatform images pushed successfully"

# Run Docker container locally
docker-run:
    @echo "Running {{DOCKER_IMAGE}}:{{VERSION}}..."
    docker run --rm -it \
        -p 8080:8080 \
        -p 9300:9300 \
        -e OTEL_ENABLED=false \
        {{DOCKER_IMAGE}}:{{VERSION}}

# Remove local Docker images
docker-clean:
    @echo "Removing local Docker images..."
    -docker rmi {{DOCKER_IMAGE}}:{{VERSION}} 2>/dev/null || true
    @echo "Docker images removed"
