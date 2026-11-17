# kpr development tasks

# Version always carries the short HEAD: a dirty tree reuses its
# tag, so the hash is what tells builds apart. The sed prefixes
# "-g" only when rev-parse prints (no git → no suffix, still "dev").
VERSION := `git describe --tags --always --dirty 2>/dev/null || echo "dev"` + `git rev-parse --short HEAD 2>/dev/null | sed 's/^/-g/'`
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

# Canonical Go version, single variable: .go-version feeds the test
# image, the compose builder arg, and CI's go-version-file alike —
# flip it there and everything follows. go.mod stays a floor.
# Exported so every compose invocation below (up and down alike)
# interpolates — a missing value fails in the compose file with
# a sentence, never in buildx with a parse error.
go_version := `cat .go-version`
export GO_VERSION := go_version

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
    OPTIONAL=(golangci-lint gotestsum gremlins node pnpm)

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
            # gotestsum, gremlins, node, and pnpm take --version;
            # golangci-lint takes version.
            case "$tool" in
                gotestsum|gremlins|node|pnpm)
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

# Build all platform binaries (plus checksums report, like make build-all)
build-all: build-linux-amd64 build-linux-arm64 build-linux-arm build-darwin-amd64 build-darwin-arm64
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p dist
    echo "Building checksums..."
    (cd dist && shasum -a 256 kpr-* > checksums.txt)
    echo "All builds complete:"
    ls -lh dist/
    cat dist/checksums.txt

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
    @echo "Release built: dist/"

# Run the servers locally
run:
    go run ./cmd/app serve

# Capture a CPU profile from a serve running with KPR_PPROF_ADDR set
# (see docs/PROFILING.md), then print the top callers. go tool pprof
# fetches and symbolizes straight from the URL — no local binary needed.
pprof-cpu addr="127.0.0.1:6060" seconds="30":
    go tool pprof -top -nodecount=25 'http://{{addr}}/debug/pprof/profile?seconds={{seconds}}'

# Same, for the heap: what the process holds, not what it burns.
pprof-heap addr="127.0.0.1:6060":
    go tool pprof -top -nodecount=25 'http://{{addr}}/debug/pprof/heap'

# Build the SvelteKit frontends and copy the static output into the
# Go embed dirs (needs pnpm — `mise install` provides node/pnpm, see
# mise.toml; also see web/README.md). The copies are committed, so
# go build needs no node and CI never runs pnpm.
web-build:
    #!/usr/bin/env bash
    set -euo pipefail
    pnpm --dir web install
    pnpm --dir web -r exec svelte-kit sync
    pnpm --dir web -r check
    pnpm --dir web -r build
    rm -rf internal/console/static internal/app/static
    mkdir -p internal/console/static internal/app/static
    cp -r web/console/build/. internal/console/static/
    cp -r web/app/build/. internal/app/static/
    echo "Frontends copied to the Go embed dirs"

# Format code
fmt:
    go fmt ./...

# Check code formatting
fmt-check:
    #!/usr/bin/env bash
    if [ -n "$(gofmt -l .)" ]; then
        echo "Go code is not formatted:"
        gofmt -d .
        exit 1
    fi

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

# Run end-to-end scenarios (testcontainers: real redis + registry, needs docker)
e2e:
    #!/usr/bin/env bash
    go test -race -tags e2e ./test/e2e/ -count=1 -v

# Validate codecov.yml against Codecov's own validator (needs network)
codecov-validate:
    #!/usr/bin/env bash
    curl -fsSL --data-binary @codecov.yml https://codecov.io/validate | grep -q '^Valid!' && echo "codecov.yml: valid"

# Union of unit + e2e coverage over product packages (needs docker)
coverage-e2e:
    #!/usr/bin/env bash
    go test -race -tags e2e -coverpkg='./internal/...,./cmd/...' -coverprofile=coverage-e2e.out ./...
    go tool cover -func=coverage-e2e.out | tail -n 5

# Browser suite (chromium, manual like mutation; `e2e` stays reserved for the Go container scenarios)
web-e2e: web-build
    #!/usr/bin/env bash
    set -euo pipefail
    rm -rf web/e2e/.data
    # Standalone package (own lockfile, not a workspace member,
    # so the -r recipes above never touch it): install, browsers,
    # run. Browsers skip when already present.
    (cd web/e2e && pnpm install --ignore-workspace)
    (cd web/e2e && pnpm exec playwright install chromium)
    # Linux additionally needs OS libraries: same install with
    # --with-deps where root or passwordless sudo exists, else a
    # warning and the plain install stands (fails loudly later
    # if the libs are actually missing).
    if [ "$(uname)" = Linux ]; then
        if [ "$(id -u)" -eq 0 ]; then
            (cd web/e2e && pnpm exec playwright install --with-deps chromium)
        elif sudo -n true 2>/dev/null; then
            (cd web/e2e && sudo -n env "PATH=$PATH" pnpm exec playwright install --with-deps chromium)
        else
            echo "WARNING: no root or passwordless sudo; hoping the image already carries the browser libraries"
        fi
    fi
    (cd web/e2e && pnpm exec playwright test)

# Fail fast when /dev/null is no character device
check-devnull:
    #!/usr/bin/env bash
    ls -l /dev/null /dev/zero /dev/full /dev/urandom /dev/stdin 2>&1 || true
    test -c /dev/null || { echo "no /dev/null character device" >&2; exit 1; }

# Run tests with coverage (coverprofile + terminal summary + HTML report)
# Device nodes gate before the run (broken runner fails fast)
# and report after it (diagnostic only, never gates).
coverage: check-devnull
    #!/usr/bin/env bash
    set -euo pipefail
    status=0
    if command -v gotestsum &> /dev/null; then
        gotestsum {{gotestsum_flags}} --format pkgname -- -race -coverprofile=coverage.out ./... || status=$?
    else
        echo "gotestsum not found, using go test..."
        echo "Install gotestsum for a dense pass/fail summary:"
        echo "  go install gotest.tools/gotestsum@latest"
        echo ""
        go test -race -coverprofile=coverage.out ./... || status=$?
    fi
    just check-devnull || true
    # Sub-90% functions, worst last; total closes the summary.
    go tool cover -func=coverage.out | grep -v '^total:' | sort -k3 -rn | awk '$3+0 < 90'
    go tool cover -func=coverage.out | grep '^total:'
    go tool cover -html=coverage.out -o coverage.html
    echo "Coverage report: coverage.html"
    exit $status

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
    # The full per-mutant stream tees to gremlins.log
    # (gitignored): triage reads the file, never a tail pipe.
    gremlins unleash --timeout-coefficient=100 --workers={{workers}} \
        --threshold-efficacy=90 --threshold-mcover=85 \
        --exclude-files 'test/e2e/(clients|scenario|fixture|toolbox)\.go$' \
        --exclude-files 'internal/storetest/contract\.go$' . 2>&1 | tee gremlins.log

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

# Docker image used by test-linux*. Tracks .go-version closely
# enough for chasing Linux-only flakes; not meant to byte-for-byte match
# the Forgejo runner image.
linux_test_image := "golang:" + go_version
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
    VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")$(git rev-parse --short HEAD 2>/dev/null | sed 's/^/-g/')
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

# Install binary (root: /usr/local/bin, otherwise ~/.local/bin).
# No sudo inside: run `sudo just install` for the system path.
install: build
    #!/usr/bin/env bash
    set -euo pipefail
    if [ "$(id -u)" -eq 0 ]; then dest=/usr/local/bin; else dest="$HOME/.local/bin"; fi
    mkdir -p "$dest"
    GOOS=$(go env GOOS)
    GOARCH=$(go env GOARCH)
    echo "Installing kpr to $dest..."
    cp dist/kpr-$GOOS-$GOARCH "$dest/kpr"
    echo "Installed successfully"
    case ":$PATH:" in *":$dest:"*) ;; *) echo "Note: $dest is not on PATH";; esac

# Remove binary (same location install would use)
uninstall:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ "$(id -u)" -eq 0 ]; then dest=/usr/local/bin; else dest="$HOME/.local/bin"; fi
    echo "Removing kpr from $dest..."
    rm -f "$dest/kpr"
    echo "Uninstalled successfully"

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

    echo "Multiplatform images pushed successfully:"
    echo "  {{DOCKER_IMAGE}}:{{VERSION}}"

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

# Hub image (Docker Hub): noroutine/kpr, sha-tagged. Push only the
# tag image-hub-test proves — never an untested build.
HUB_IMAGE := env_var_or_default('HUB_IMAGE', 'noroutine/kpr')
HUB_TAG := env_var_or_default('HUB_TAG', `git rev-parse --short HEAD`)

# Build the Hub image, sha-tagged, local arch
image-hub:
    docker build -f Dockerfile -t {{HUB_IMAGE}}:{{HUB_TAG}} .

# Prove the tagged Hub image on a throwaway file-backend stack
# extracted from docs/QUICKSTART.md (ports remapped off the dev
# stack): unlock, ttl push, reap marks, sweep dry-runs, console healthy
image-hub-test:
    #!/usr/bin/env bash
    set -euo pipefail
    python3 -c "import re;md=open('docs/QUICKSTART.md').read();b=re.findall(r'''\`\`\`yaml\n(.*?)\`\`\`''',md,re.S);open('/tmp/kpr-hubtest-src.yml','w').write(b[0]);open('/tmp/kpr-hubtest-reg.yml','w').write(b[1])"
    sed -e 's|noroutine/kpr:<release-tag>|{{HUB_IMAGE}}:{{HUB_TAG}}|' \
        -e 's|"5000:5000"|"5003:5000"|' -e 's|"9300:9300"|"9301:9300"|' \
        -e 's|\./registry-config.yml|/tmp/kpr-hubtest-reg.yml|' \
        /tmp/kpr-hubtest-src.yml > /tmp/kpr-hubtest-compose.yml
    docker compose -f /tmp/kpr-hubtest-compose.yml -p kprhubtest up -d
    trap 'docker compose -f /tmp/kpr-hubtest-compose.yml -p kprhubtest down -v' EXIT
    KPR=kprhubtest-kpr-1
    docker exec -u 0 "$KPR" chown -R 1000:1000 /var/lib/registry
    docker exec "$KPR" kpr store unlock
    crane copy busybox:latest localhost:5003/qs/hello:10s
    docker exec "$KPR" kpr status | grep -q 'tracked'
    docker exec "$KPR" kpr plan
    sleep 12
    docker exec "$KPR" kpr reap --no-dry-run | grep -q 'ttl:10s elapsed'
    docker exec "$KPR" kpr sweep
    curl -sf localhost:9301/health
    echo "HUB IMAGE OK: {{HUB_IMAGE}}:{{HUB_TAG}}"

# Push the sha tag multiplatform to Docker Hub (needs `docker login`).
# Same Dockerfile and sources as the tested image, so the local-arch
# slice is the proven bytes; the other arches share them.
image-hub-push:
    docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 \
        -f Dockerfile -t {{HUB_IMAGE}}:{{HUB_TAG}} --push .

# Stack backend: file (default) or redis. One knob switches every
# compose recipe below: `COMPOSE=redis just up` brings up the
# redis-backed stack instead. No per-backend recipes — the shape is
# identical, only the state backend differs (see docs/STORES.md).
COMPOSE := env_var_or_default('COMPOSE', 'file')
COMPOSE_FILE := if COMPOSE == 'redis' { 'docker-compose.redis.yml' } else if COMPOSE == 'file' { 'docker-compose.yml' } else { error('COMPOSE must be file or redis, got ' + COMPOSE) }

# Start full dev stack locally (detached) — base plus the observability and shadow overlays
up: dev-certs
    @if curl -s -m 3 -D - -o /dev/null http://localhost:5000/v2/ 2>/dev/null | grep -qi airtunes; then echo "WARNING: localhost:5000 answers like macOS AirPlay Receiver (Server: AirTunes) — turn it off in System Settings → General → AirDrop & Handoff and retry"; fi
    docker compose -f {{COMPOSE_FILE}} -f docker-compose.observability.yml -f docker-compose.shadow.yml up -d --build
    docker compose -f {{COMPOSE_FILE}} run --rm -u 0 --no-deps --entrypoint chown kpr -R 1000:1000 /var/lib/registry /var/lib/kpr >/dev/null 2>&1 || echo "WARNING: shared store claim failed (fresh volumes arrive root-owned; both writers run as uid 1000)"

# Start the base dev stack only (no overlays)
up-minimal: dev-certs
    @if curl -s -m 3 -D - -o /dev/null http://localhost:5000/v2/ 2>/dev/null | grep -qi airtunes; then echo "WARNING: localhost:5000 answers like macOS AirPlay Receiver (Server: AirTunes) — turn it off in System Settings → General → AirDrop & Handoff and retry"; fi
    docker compose -f {{COMPOSE_FILE}} up -d --build
    docker compose -f {{COMPOSE_FILE}} run --rm -u 0 --no-deps --entrypoint chown kpr -R 1000:1000 /var/lib/registry /var/lib/kpr >/dev/null 2>&1 || echo "WARNING: shared store claim failed (fresh volumes arrive root-owned; both writers run as uid 1000)"

# Start dev stack behind a Traefik front: TLS terminates on the
# local certbot identity, the app serves at its hostname (detached)
up-traefik:
    @if curl -s -m 3 -D - -o /dev/null http://localhost:5000/v2/ 2>/dev/null | grep -qi airtunes; then echo "WARNING: localhost:5000 answers like macOS AirPlay Receiver (Server: AirTunes) — turn it off in System Settings → General → AirDrop & Handoff and retry"; fi
    GO_VERSION={{go_version}} docker compose -f {{COMPOSE_FILE}} -f docker-compose.traefik.yml up -d --build
    docker compose -f {{COMPOSE_FILE}} -f docker-compose.traefik.yml run --rm -u 0 --no-deps --entrypoint chown kpr -R 1000:1000 /var/lib/registry /var/lib/kpr >/dev/null 2>&1 || echo "WARNING: shared store claim failed (fresh volumes arrive root-owned; both writers run as uid 1000)"

# Start dev stack with the TLS overlay: console, app, receiver, and
# edge serve HTTPS with the staged self-signed identity (detached)
up-tls: dev-certs
    @if curl -s -m 3 -D - -o /dev/null http://localhost:5000/v2/ 2>/dev/null | grep -qi airtunes; then echo "WARNING: localhost:5000 answers like macOS AirPlay Receiver (Server: AirTunes) — turn it off in System Settings → General → AirDrop & Handoff and retry"; fi
    docker compose -f {{COMPOSE_FILE}} -f docker-compose.tls.yml up -d --build
    docker compose -f {{COMPOSE_FILE}} -f docker-compose.tls.yml run --rm -u 0 --no-deps --entrypoint chown kpr -R 1000:1000 /var/lib/registry /var/lib/kpr >/dev/null 2>&1 || echo "WARNING: shared store claim failed (fresh volumes arrive root-owned; both writers run as uid 1000)"

# Refresh dev TLS staging from scripts/dev-certs.sh.
# Rerun after deleting a fixture.
dev-certs:
    bash scripts/dev-certs.sh

# Stop local stacks (base plus every overlay)
down:
    docker compose -f {{COMPOSE_FILE}} -f docker-compose.observability.yml -f docker-compose.shadow.yml down

# Garbage-collect unreferenced registry blobs (API deletes drop the
# tag reference only; --delete-untagged also drops the orphaned
# manifest revisions that would otherwise keep every blob alive).
# Runs offline with the service's own volumes, then restarts the
# registry. Takes the shared collector lock first: a kpr gc run in
# flight refuses this, and vice versa. Manual collector runs bypass
# the lock (the registry itself sets none) — don't run those
# concurrently either.
#
# File backend: the collector runs inside the kpr container (which
# embeds /bin/registry and mounts the same volumes) under flock on
# the real gc lock file — blocking, not failing, since file locks
# die with their holder and can never go stale.
gc:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ "{{COMPOSE}}" = "redis" ]; then
      docker exec kpr-redis redis-cli -a "${REDIS_PASSWORD:-kpr-dev-only}" -n 4 SET kpr:gc:lock make-gc NX EX 1800 2>/dev/null | grep -q OK || { echo "kpr:gc:lock held (kpr gc running?) or redis unreachable — wait it out, or DEL kpr:gc:lock on DB 4 if stale"; exit 1; }
      docker compose -f {{COMPOSE_FILE}} stop registry
      docker compose -f {{COMPOSE_FILE}} run --rm --no-deps --entrypoint /bin/registry registry garbage-collect --delete-untagged /etc/distribution/config.yml
      docker compose -f {{COMPOSE_FILE}} start registry
      docker exec kpr-redis redis-cli -a "${REDIS_PASSWORD:-kpr-dev-only}" -n 4 DEL kpr:gc:lock || true
    else
      docker compose -f {{COMPOSE_FILE}} stop registry
      docker exec kpr mkdir -p /var/lib/kpr/locks
      echo "waiting for the gc lock (a kpr gc run in flight holds it)…"
      docker exec kpr flock /var/lib/kpr/locks/kpr:gc:lock.lock /bin/registry garbage-collect --delete-untagged /etc/distribution/config.yml
      docker compose -f {{COMPOSE_FILE}} start registry
    fi
