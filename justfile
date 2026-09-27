# kpr development tasks

VERSION := `git describe --tags --always --dirty 2>/dev/null || echo "dev"`
COMMIT := `git rev-parse --short HEAD 2>/dev/null || echo "unknown"`
BUILD_TIME := `date -u +%Y-%m-%dT%H:%M:%SZ`
PKG := "nrtn.dev/catalyst/kpr"
LDFLAGS := "-s -w -X " + PKG + "/internal/config.Version=" + VERSION + " -X " + PKG + "/internal/config.Commit=" + COMMIT + " -X " + PKG + "/internal/config.BuildTime=" + BUILD_TIME

# Act/Forgejo runners set CI=true; disable gotestsum's ANSI colors there
# since raw escape codes just clutter the step log.
gotestsum_flags := if env("CI", "") == "true" { "--no-color" } else { "" }

# Pinned gremlins (mutation testing) version. go install keeps it out of
# go.mod/go.sum; same pattern as gotestsum above.
GREMLINS_VERSION := "v0.6.0"

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
    # Optional tools (for development) — optional but hard to live
    # without; install-prereqs installs all of them.
    OPTIONAL=(golangci-lint gotestsum gremlins)

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
            # gotestsum and gremlins take --version; golangci-lint takes version.
            case "$tool" in
                gotestsum|gremlins)
                    version=$($tool --version 2>&1 | head -n1 || echo "installed")
                    ;;
                *)
                    version=$($tool version 2>&1 | head -n1 || echo "installed")
                    ;;
            esac
            echo "  ✓ $tool - $version"
        else
            echo "  - $tool - not installed (optional)"
        fi
    done

    echo ""
    if [ "$all_good" = true ]; then
        echo "✓ All required prerequisites are installed!"
        echo ""
        echo "To install optional tools, run: just install-prereqs"
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
    cd dist && shasum -a 256 kpr-* > checksums.txt
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

# Lint code (golangci-lint v2 if available, otherwise go vet)
lint:
    #!/usr/bin/env bash
    # .golangci.yml is v2 syntax: a v1 binary would fail on it, and plain
    # absence means the same fallback — both degrade to go vet.
    if command -v golangci-lint &> /dev/null && ! golangci-lint version 2>&1 | grep -q " version v1\."; then
        echo "Running golangci-lint..."
        golangci-lint run
    else
        echo "golangci-lint v2 not found, using go vet..."
        echo "Install golangci-lint v2 for better linting:"
        echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"
        echo ""
        go vet ./...
    fi

# Run go vet
vet:
    go vet ./...

# Run tests (gotestsum if available, for a dense per-package pass/fail summary)
test:
    #!/usr/bin/env bash
    if command -v gotestsum &> /dev/null; then
        gotestsum {{gotestsum_flags}} --format pkgname -- -race ./...
    else
        echo "gotestsum not found, using go test..."
        echo "Install gotestsum for a dense pass/fail summary:"
        echo "  go install gotest.tools/gotestsum@latest"
        echo ""
        go test -v -race ./...
    fi

# Run tests with coverage (coverprofile + terminal summary + HTML report)
coverage:
    #!/usr/bin/env bash
    if command -v gotestsum &> /dev/null; then
        gotestsum {{gotestsum_flags}} --format pkgname -- -race -coverprofile=coverage.out ./...
    else
        echo "gotestsum not found, using go test..."
        echo "Install gotestsum for a dense pass/fail summary:"
        echo "  go install gotest.tools/gotestsum@latest"
        echo ""
        go test -race -coverprofile=coverage.out ./...
    fi
    go tool cover -func=coverage.out | tail -n 20
    go tool cover -html=coverage.out -o coverage.html
    echo "Coverage report: coverage.html"

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

# Mutation testing (gremlins). Slow by design (minutes) — periodic and
# on-demand, never part of check or per-push CI. --timeout-coefficient
# is high on purpose: gremlins derives each mutant's timeout from its
# covering tests' own milliseconds, and a test binary can't start and
# serve in that budget (everything spuriously TIMED OUT at the default
# coefficient of 3). --workers caps parallelism so hungry test runs
# don't starve each other into timeouts.
mutation workers="4":
    #!/usr/bin/env bash
    set -euo pipefail
    if ! command -v gremlins &> /dev/null; then
        echo "gremlins not found. Install it (pinned {{GREMLINS_VERSION}}, stays out of go.mod):"
        echo "  go install github.com/go-gremlins/gremlins/cmd/gremlins@{{GREMLINS_VERSION}}"
        exit 1
    fi
    gremlins unleash --timeout-coefficient=100 --workers={{workers}} \
        --threshold-efficacy=90 --threshold-mcover=85 .

# Discover mutation candidates without running any tests
mutation-dry:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! command -v gremlins &> /dev/null; then
        echo "gremlins not found. Install it (pinned {{GREMLINS_VERSION}}, stays out of go.mod):"
        echo "  go install github.com/go-gremlins/gremlins/cmd/gremlins@{{GREMLINS_VERSION}}"
        exit 1
    fi
    gremlins unleash --dry-run .

# Docker image used by test-linux*. Tracks go.mod's `go` directive closely
# enough for chasing Linux-only flakes; not meant to byte-for-byte match
# the Forgejo runner image.
linux_test_image := "golang:1.27"
# Named volumes (not a bind mount) so module downloads and build cache
# persist between runs without polluting the host's own Go caches or the
# repo working tree with root-owned files written by the container.
linux_test_mod_cache := "kpr-linux-test-gomod"
linux_test_build_cache := "kpr-linux-test-gobuild"

# Run the race+coverage suite in a Linux container (for Linux/filesystem-only flakes)
test-linux:
    docker run --rm -v "{{justfile_directory()}}:/work" -w /work \
        -v {{linux_test_mod_cache}}:/go/pkg/mod \
        -v {{linux_test_build_cache}}:/root/.cache/go-build \
        -e GOFLAGS=-mod=mod \
        {{linux_test_image}} \
        sh -c 'go test -race -coverprofile=coverage.out ./...'

# Same as test-linux but -v, for reading full output on a failure
test-linux-verbose:
    docker run --rm -v "{{justfile_directory()}}:/work" -w /work \
        -v {{linux_test_mod_cache}}:/go/pkg/mod \
        -v {{linux_test_build_cache}}:/root/.cache/go-build \
        -e GOFLAGS=-mod=mod \
        {{linux_test_image}} \
        sh -c 'go test -v -race -coverprofile=coverage.out ./...'

# Run the suite N times back to back (default 20) to chase a flake
test-linux-repeat n="20":
    docker run --rm -v "{{justfile_directory()}}:/work" -w /work \
        -v {{linux_test_mod_cache}}:/go/pkg/mod \
        -v {{linux_test_build_cache}}:/root/.cache/go-build \
        -e GOFLAGS=-mod=mod \
        {{linux_test_image}} \
        sh -c 'go test -race -count={{n}} ./...'

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
check: fmt-check vet lint test

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

# Verify module dependencies
verify:
    go mod verify

# Install optional-but-essential dev tools (lint, test runner, mutants)
install-prereqs:
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
    go install gotest.tools/gotestsum@latest
    go install github.com/go-gremlins/gremlins/cmd/gremlins@{{GREMLINS_VERSION}}

# Install the CI tool set (same tools; keeps CI output rich instead of
# fallback blurbs). Both Forgejo workflows call this.
install-ci-prereqs: install-prereqs

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
