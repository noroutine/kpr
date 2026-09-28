.PHONY: all build build-all clean clean-dist test e2e coverage coverage-e2e coverage-report bench mutation mutation-dry test-linux test-linux-verbose test-linux-repeat fmt fmt-check check fix vet lint run release help prereqs deps verify install-prereqs install-ci-prereqs install uninstall version info docker-build docker-build-multiplatform docker-run docker-clean up up-observability down gc
.DEFAULT_GOAL := help

# Version information
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Build variables
BINARY_NAME := kpr
PKG := nrtn.dev/catalyst/kpr
LDFLAGS := -s -w \
	-X $(PKG)/internal/config.Version=$(VERSION) \
	-X $(PKG)/internal/config.Commit=$(COMMIT) \
	-X $(PKG)/internal/config.BuildTime=$(BUILD_TIME)

# Act/Forgejo runners set CI=true; disable gotestsum's ANSI colors there
# since raw escape codes just clutter the step log.
ifeq ($(CI),true)
GOTESTSUM_FLAGS := --no-color
endif

# Directories
DIST_DIR := dist
CMD_DIR := ./cmd/app

# Source files for dependency tracking
GO_FILES := $(shell find . -name '*.go' -type f 2>/dev/null)
GO_MOD_FILES := go.mod go.sum

# Build targets
PLATFORMS := linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64
PLATFORM_BINARIES := $(foreach platform,$(PLATFORMS),$(DIST_DIR)/$(BINARY_NAME)-$(subst /,-,$(platform)))

## help: Show this help message
help:
	@echo 'Usage:'
	@echo '  make <target>'
	@echo ''
	@echo 'Targets:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'

## prereqs: Check build prerequisites
prereqs:
	@echo "Checking build prerequisites..."
	@echo ""
	@echo "Required tools (for building):"
	@command -v go >/dev/null 2>&1 && echo "  ✓ go - $$(go version)" || (echo "  ✗ go - NOT FOUND" && exit 1)
	@command -v git >/dev/null 2>&1 && echo "  ✓ git - $$(git version)" || (echo "  ✗ git - NOT FOUND" && exit 1)
	@echo ""
	@echo "Optional tools (optional but hard to live without; make install-prereqs installs all of them):"
	@command -v golangci-lint >/dev/null 2>&1 && echo "  ✓ golangci-lint - $$(golangci-lint version 2>&1 | head -n1)" || echo "  - golangci-lint - not installed (optional)"
	@command -v gotestsum >/dev/null 2>&1 && echo "  ✓ gotestsum - $$(gotestsum --version 2>&1 | head -n1)" || echo "  - gotestsum - not installed (optional)"
	@command -v gremlins >/dev/null 2>&1 && echo "  ✓ gremlins - $$(gremlins --version 2>&1 | head -n1)" || echo "  - gremlins - not installed (optional)"
	@echo ""
	@echo "✓ All required prerequisites are installed!"
	@echo "To install optional tools, run: make install-prereqs"

## all: Build for all platforms
all: build-all

## build: Build binary for current platform
build:
	@echo "Building $(BINARY_NAME) $(VERSION)..."
	@mkdir -p $(DIST_DIR)
	@GOOS=$$(go env GOOS); GOARCH=$$(go env GOARCH); \
		CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)-$$GOOS-$$GOARCH $(CMD_DIR); \
		echo "Built: $(DIST_DIR)/$(BINARY_NAME)-$$GOOS-$$GOARCH"

## build-all: Build binaries for all platforms
build-all: $(PLATFORM_BINARIES)
	@echo "Building checksums..."
	@cd $(DIST_DIR) && shasum -a 256 $(BINARY_NAME)-* > checksums.txt
	@echo "All builds complete:"
	@ls -lh $(DIST_DIR)/
	@cat $(DIST_DIR)/checksums.txt

$(DIST_DIR)/$(BINARY_NAME)-linux-amd64: $(GO_FILES) $(GO_MOD_FILES)
	@mkdir -p $(DIST_DIR)
	@echo "Building linux/amd64..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $@ $(CMD_DIR)

$(DIST_DIR)/$(BINARY_NAME)-linux-arm64: $(GO_FILES) $(GO_MOD_FILES)
	@mkdir -p $(DIST_DIR)
	@echo "Building linux/arm64..."
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $@ $(CMD_DIR)

$(DIST_DIR)/$(BINARY_NAME)-linux-arm: $(GO_FILES) $(GO_MOD_FILES)
	@mkdir -p $(DIST_DIR)
	@echo "Building linux/arm (armv7)..."
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o $@ $(CMD_DIR)

$(DIST_DIR)/$(BINARY_NAME)-darwin-amd64: $(GO_FILES) $(GO_MOD_FILES)
	@mkdir -p $(DIST_DIR)
	@echo "Building darwin/amd64..."
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $@ $(CMD_DIR)

$(DIST_DIR)/$(BINARY_NAME)-darwin-arm64: $(GO_FILES) $(GO_MOD_FILES)
	@mkdir -p $(DIST_DIR)
	@echo "Building darwin/arm64..."
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $@ $(CMD_DIR)

## test: Run tests (gotestsum if available, for a dense per-package pass/fail summary)
test:
	@if command -v gotestsum >/dev/null 2>&1; then \
		gotestsum $(GOTESTSUM_FLAGS) --format pkgname -- -race ./...; \
	else \
		echo "gotestsum not found, using go test..."; \
		echo "Install gotestsum for a dense pass/fail summary:"; \
		echo "  go install gotest.tools/gotestsum@latest"; \
		echo ""; \
		go test -v -race ./...; \
	fi

## e2e: Run end-to-end scenarios (testcontainers: real redis + registry, needs docker)
e2e:
	go test -race -tags e2e ./test/e2e/ -count=1 -v

## coverage: Run tests with coverage (coverprofile + terminal summary + HTML report)
coverage:
	@if command -v gotestsum >/dev/null 2>&1; then \
		gotestsum $(GOTESTSUM_FLAGS) --format pkgname -- -race -coverprofile=coverage.out ./...; \
	else \
		echo "gotestsum not found, using go test..."; \
		echo "Install gotestsum for a dense pass/fail summary:"; \
		echo "  go install gotest.tools/gotestsum@latest"; \
		echo ""; \
		go test -race -coverprofile=coverage.out ./...; \
	fi
	go tool cover -func=coverage.out | tail -n 20
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## coverage-report: Reprint the coverage summary from the last run without re-running tests
coverage-report:
	@if [ ! -f coverage.out ]; then echo "No coverage.out found — run 'make coverage' first."; exit 1; fi
	go tool cover -func=coverage.out | tail -n 20

## coverage-e2e: Union of unit + e2e coverage over product packages (needs docker)
coverage-e2e:
	go test -race -tags e2e -coverpkg='./internal/...,./cmd/...' -coverprofile=coverage-e2e.out ./...
	go tool cover -func=coverage-e2e.out | tail -n 5

## bench: Run benchmarks (no tests, measurements only)
bench:
	go test -run=NONE -bench=. -benchmem ./...

# Pinned gremlins (mutation testing) version. go install keeps it out of
# go.mod/go.sum; same pattern as gotestsum.
GREMLINS_VERSION := v0.6.0
# Parallel mutant workers; override with WORKERS=... (see justfile for why
# parallelism is capped and the timeout coefficient is high).
WORKERS ?= 4

## mutation: Mutation testing (gremlins). Slow by design (minutes) — periodic and on-demand, never part of check or per-push CI
mutation:
	@if ! command -v gremlins >/dev/null 2>&1; then \
		echo "gremlins not found. Install it (pinned $(GREMLINS_VERSION), stays out of go.mod):"; \
		echo "  go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)"; \
		exit 1; \
	fi
	gremlins unleash --timeout-coefficient=100 --workers=$(WORKERS) \
		--threshold-efficacy=90 --threshold-mcover=85 .

## mutation-dry: Discover mutation candidates without running any tests
mutation-dry:
	@if ! command -v gremlins >/dev/null 2>&1; then \
		echo "gremlins not found. Install it (pinned $(GREMLINS_VERSION), stays out of go.mod):"; \
		echo "  go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)"; \
		exit 1; \
	fi
	gremlins unleash --dry-run .

# Docker image used by test-linux*. Tracks go.mod's `go` directive closely
# enough for chasing Linux-only flakes; not meant to byte-for-byte match
# the Forgejo runner image.
LINUX_TEST_IMAGE := golang:1.27
# Named volumes (not a bind mount) so module downloads and build cache
# persist between runs without polluting the host's own Go caches or the
# repo working tree with root-owned files written by the container.
LINUX_TEST_MOD_CACHE := kpr-linux-test-gomod
LINUX_TEST_BUILD_CACHE := kpr-linux-test-gobuild

## test-linux: Run the race+coverage suite in a Linux container (for Linux/filesystem-only flakes)
test-linux:
	docker run --rm -v "$(CURDIR):/work" -w /work \
		-v $(LINUX_TEST_MOD_CACHE):/go/pkg/mod \
		-v $(LINUX_TEST_BUILD_CACHE):/root/.cache/go-build \
		-e GOFLAGS=-mod=mod \
		$(LINUX_TEST_IMAGE) \
		sh -c 'go test -race -coverprofile=coverage.out ./...'

## test-linux-verbose: Same as test-linux but -v, for reading full output on a failure
test-linux-verbose:
	docker run --rm -v "$(CURDIR):/work" -w /work \
		-v $(LINUX_TEST_MOD_CACHE):/go/pkg/mod \
		-v $(LINUX_TEST_BUILD_CACHE):/root/.cache/go-build \
		-e GOFLAGS=-mod=mod \
		$(LINUX_TEST_IMAGE) \
		sh -c 'go test -v -race -coverprofile=coverage.out ./...'

## test-linux-repeat: Run test-linux N times back to back (default 20; override with N=...) to chase a flake
N ?= 20
test-linux-repeat:
	docker run --rm -v "$(CURDIR):/work" -w /work \
		-v $(LINUX_TEST_MOD_CACHE):/go/pkg/mod \
		-v $(LINUX_TEST_BUILD_CACHE):/root/.cache/go-build \
		-e GOFLAGS=-mod=mod \
		$(LINUX_TEST_IMAGE) \
		sh -c 'go test -race -count=$(N) ./...'

## test-docker: Test Docker image
test-docker:
	@set -e; \
	VERSION=$$(git describe --tags --always --dirty 2>/dev/null || echo "dev"); \
	HOST="[::1]"; \
	echo "Cleaning up any existing container..."; \
	docker rm -f kpr-test 2>/dev/null || true; \
	echo "Starting container..."; \
	docker run -d --name kpr-test \
		-p 8080:8080 -p 9300:9300 \
		$(DOCKER_IMAGE):$$VERSION; \
	echo ""; \
	echo "Waiting for container to start..."; \
	sleep 5; \
	echo ""; \
	echo "Container status:"; \
	docker ps -a | grep kpr-test || echo "Container not found!"; \
	echo ""; \
	echo "Container inspect:"; \
	docker inspect --format='{{.State.Status}} - ExitCode={{.State.ExitCode}} {{if .State.Error}}- Error={{.State.Error}}{{end}}' kpr-test || echo "Failed to inspect"; \
	echo ""; \
	echo "Container logs:"; \
	docker logs kpr-test 2>&1 || echo "Failed to get logs"; \
	echo ""; \
	echo "Testing endpoints from inside container..."; \
	HEALTH_OK=false; \
	APP_OK=false; \
	echo ""; \
	echo "Testing health endpoint (IPv6 [::1]:9300)..."; \
	if docker exec kpr-test wget -q -O- http://[::1]:9300/health >/dev/null 2>&1; then \
		echo "✓ IPv6 health check passed"; \
		HEALTH_OK=true; \
	else \
		echo "✗ IPv6 health check failed"; \
	fi; \
	echo "Testing health endpoint (IPv4 127.0.0.1:9300)..."; \
	if docker exec kpr-test wget -q -O- http://127.0.0.1:9300/health >/dev/null 2>&1; then \
		echo "✓ IPv4 health check passed"; \
		HEALTH_OK=true; \
	else \
		echo "✗ IPv4 health check failed"; \
	fi; \
	echo ""; \
	echo "Testing app endpoint (IPv6 [::1]:8080)..."; \
	if docker exec kpr-test wget -q -O- http://[::1]:8080/ >/dev/null 2>&1; then \
		echo "✓ IPv6 app endpoint passed"; \
		APP_OK=true; \
	else \
		echo "✗ IPv6 app endpoint failed"; \
	fi; \
	echo "Testing app endpoint (IPv4 127.0.0.1:8080)..."; \
	if docker exec kpr-test wget -q -O- http://127.0.0.1:8080/ >/dev/null 2>&1; then \
		echo "✓ IPv4 app endpoint passed"; \
		APP_OK=true; \
	else \
		echo "✗ IPv4 app endpoint failed"; \
	fi; \
	echo ""; \
	echo "Final container logs:"; \
	docker logs kpr-test 2>&1 || echo "Failed to get logs"; \
	echo ""; \
	echo "Stopping and removing container..."; \
	docker stop kpr-test 2>/dev/null || echo "Container already stopped"; \
	docker rm kpr-test 2>/dev/null || echo "Failed to remove container"; \
	if [ "$$HEALTH_OK" = false ] || [ "$$APP_OK" = false ]; then \
		echo ""; \
		echo "Tests failed!"; \
		exit 1; \
	fi; \
	echo ""; \
	echo "All tests passed!"

## fmt: Format code
fmt:
	go fmt ./...

## fmt-check: Check code formatting
fmt-check:
	@if [ -n "$$(gofmt -l .)" ]; then \
		echo "Go code is not formatted:"; \
		gofmt -d .; \
		exit 1; \
	fi

## check: Run all checks (format check + vet + lint + test)
check: fmt-check vet lint test

## fix: Fix all auto-fixable issues
fix: fmt

## vet: Run go vet
vet:
	go vet ./...

## lint: Lint code (golangci-lint v2 if available, otherwise go vet)
lint:
	@if command -v golangci-lint >/dev/null 2>&1 && ! golangci-lint version 2>&1 | grep -q " version v1\."; then \
		echo "Running golangci-lint..."; \
		golangci-lint run; \
	else \
		echo "golangci-lint v2 not found, using go vet..."; \
		echo "Install golangci-lint v2 for better linting:"; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		echo ""; \
		go vet ./...; \
	fi

## clean: Remove build artifacts
clean:
	rm -f coverage.out coverage.html
	go clean

## clean-dist: Remove dist directory
clean-dist:
	rm -rf $(DIST_DIR)

## run: Run the servers locally
run:
	go run $(CMD_DIR) serve

## release: Build release archives for all platforms
release: clean-dist
	@echo "Building release $(VERSION)..."
	@mkdir -p $(DIST_DIR)
	$(MAKE) build-all
	@echo "Release built: $(DIST_DIR)/"

## deps: Download and tidy dependencies
deps:
	go mod download
	go mod tidy

## verify: Verify module dependencies
verify:
	go mod verify

## install-prereqs: Install optional-but-essential dev tools (lint, test runner, mutants)
install-prereqs:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install gotest.tools/gotestsum@latest
	go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)

## install-ci-prereqs: Install the CI tool set (same tools; keeps CI output rich instead of fallback blurbs). Both Forgejo workflows call this
install-ci-prereqs: install-prereqs

## install: Install binary to /usr/local/bin
install: build
	@echo "Installing $(BINARY_NAME) to /usr/local/bin..."
	@GOOS=$$(go env GOOS); GOARCH=$$(go env GOARCH); \
		sudo cp $(DIST_DIR)/$(BINARY_NAME)-$$GOOS-$$GOARCH /usr/local/bin/$(BINARY_NAME)
	@echo "Installed successfully"

## uninstall: Remove binary from /usr/local/bin
uninstall:
	@echo "Removing $(BINARY_NAME) from /usr/local/bin..."
	sudo rm -f /usr/local/bin/$(BINARY_NAME)
	@echo "Uninstalled successfully"

## version: Show version information
version:
	@echo "Version:    $(VERSION)"
	@echo "Commit:     $(COMMIT)"
	@echo "Build Time: $(BUILD_TIME)"
	@echo ""
	@go version

## info: Show module info
info:
	@echo "Version:    $(VERSION)"
	@echo "Commit:     $(COMMIT)"
	@echo "Build Time: $(BUILD_TIME)"
	@echo ""
	@go version
	@echo ""
	@go list -m all

# Docker configuration
DOCKER_REGISTRY ?= nrtn.dev/catalyst
DOCKER_IMAGE ?= $(DOCKER_REGISTRY)/$(BINARY_NAME)

## docker-build: Build Docker image for current platform
docker-build:
	@echo "Building binary for current platform..."
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux go build -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)-linux-$$(go env GOARCH) $(CMD_DIR)
	@echo "Packaging Docker image $(DOCKER_IMAGE):$(VERSION)..."
	@docker buildx build \
		-f Dockerfile.package \
		-t $(DOCKER_IMAGE):$(VERSION) \
		$$(if echo "$(VERSION)" | grep -qE '^v?[0-9]+\.[0-9]+\.[0-9]+$$'; then echo "-t $(DOCKER_IMAGE):latest"; fi) \
		--load .

## docker-build-multiplatform: Build and push multiplatform Docker images
docker-build-multiplatform: build-all
	@echo "Packaging multiplatform Docker images $(DOCKER_IMAGE):$(VERSION)..."
	@# Build all platforms at once (Dockerfile auto-picks binary based on platform)
	@docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 \
		-f Dockerfile.package \
		-t $(DOCKER_IMAGE):$(VERSION) \
		$$(if echo "$(VERSION)" | grep -qE '^v?[0-9]+\.[0-9]+\.[0-9]+$$'; then echo "-t $(DOCKER_IMAGE):latest"; fi) \
		--push .
	@echo "Multiplatform images pushed successfully:"
	@echo "  $(DOCKER_IMAGE):$(VERSION)"

## docker-run: Run Docker container locally
docker-run:
	@echo "Running $(DOCKER_IMAGE):$(VERSION)..."
	docker run --rm -it \
		-p 8080:8080 \
		-p 9300:9300 \
		-e OTEL_ENABLED=false \
		$(DOCKER_IMAGE):$(VERSION)

## docker-clean: Remove local Docker images
docker-clean:
	@echo "Removing local Docker images..."
	-docker rmi $(DOCKER_IMAGE):$(VERSION) 2>/dev/null || true
	@echo "Docker images removed"

## up: Start base dev stack locally (detached)
up:
	@if curl -s -m 3 -D - -o /dev/null http://localhost:5000/v2/ 2>/dev/null | grep -qi airtunes; then echo "WARNING: localhost:5000 answers like macOS AirPlay Receiver (Server: AirTunes) — turn it off in System Settings → General → AirDrop & Handoff and retry"; fi
	docker compose up -d --build

## up-observability: Start full dev stack with observability overlay (detached)
up-observability:
	@if curl -s -m 3 -D - -o /dev/null http://localhost:5000/v2/ 2>/dev/null | grep -qi airtunes; then echo "WARNING: localhost:5000 answers like macOS AirPlay Receiver (Server: AirTunes) — turn it off in System Settings → General → AirDrop & Handoff and retry"; fi
	docker compose -f docker-compose.yml -f docker-compose.observability.yml up -d --build

## down: Stop local stacks
down:
	docker compose -f docker-compose.yml -f docker-compose.observability.yml down

## gc: Garbage-collect unreferenced registry blobs (API deletes drop the
## tag reference only; --delete-untagged also drops the orphaned
## manifest revisions that would otherwise keep every blob alive).
## Runs offline with the service's own volumes, then restarts the
## registry. Takes the shared collector lock first: a kpr gc run in
## flight refuses this, and vice versa. Manual collector runs bypass
## the lock (the registry itself sets none) — don't run those
## concurrently either.
gc:
	@docker exec kpr-redis redis-cli -a "$${REDIS_PASSWORD:-kpr-dev-only}" -n 4 SET kpr:gc:lock make-gc NX EX 1800 2>/dev/null | grep -q OK || (echo "kpr:gc:lock held (kpr gc running?) or redis unreachable — wait it out, or DEL kpr:gc:lock on DB 4 if stale"; exit 1)
	docker compose stop registry
	docker compose run --rm --no-deps --entrypoint /bin/registry registry garbage-collect --delete-untagged /etc/distribution/config.yml
	docker compose start registry
	-docker exec kpr-redis redis-cli -a "$${REDIS_PASSWORD:-kpr-dev-only}" -n 4 DEL kpr:gc:lock
