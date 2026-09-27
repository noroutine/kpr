# Build Guide

kpr supports multiarch builds for Linux and macOS on amd64 and arm64.

## Quick Start

```bash
# Build for current platform
just build

# Build for all platforms
just build-all

# Build specific platform
just build-linux-amd64
just build-linux-arm64
just build-darwin-amd64
just build-darwin-arm64
```

## Build Tools

You can use either **justfile** or **Makefile**:

### Using just (recommended)

```bash
# Show all commands
just

# Build current platform
just build

# Build all platforms
just build-all

# Build and create release with checksums
just release

# Show version info
just info
```

### Using make

```bash
# Show all commands
make help

# Build current platform
make build

# Build all platforms
make build-all

# Install to /usr/local/bin
make install

# Show version info
make version
```

## Build Outputs

Single platform build:
```
./kpr                    # Current platform binary
```

Multiarch build:
```
dist/
├── kpr-linux-amd64     # Linux x86_64
├── kpr-linux-arm64     # Linux ARM64
├── kpr-darwin-amd64    # macOS Intel
├── kpr-darwin-arm64    # macOS Apple Silicon
└── checksums.txt           # SHA256 checksums
```

## Version Information

Version is set via git tags and ldflags:

```bash
# Check version
./kpr --version

# See build info
just info
```

Version comes from:
- Git tag if available (e.g., `v0.2.0`)
- Git commit hash if no tag (e.g., `a1b2c3d`)
- `dev` if not in git repo

## Release Process

### Manual Release

```bash
# Tag the release
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0

# Build release binaries
just release

# Outputs to dist/ with checksums
ls -lh dist/
```

### Using GoReleaser

Install goreleaser:
```bash
# macOS
brew install goreleaser

# Linux
go install github.com/goreleaser/goreleaser@latest
```

Build release:
```bash
# Test release locally (no push)
goreleaser release --snapshot --clean

# Build and publish release (requires GitHub token)
export GITHUB_TOKEN=your_token
goreleaser release --clean
```

GoReleaser will:
- Build for all platforms
- Create tar.gz archives
- Generate checksums
- Create GitHub release
- Upload binaries as release assets

## Cross-Compilation

Go makes cross-compilation easy. From any platform:

```bash
# Build Linux binary from macOS
GOOS=linux GOARCH=amd64 go build -o kpr ./cmd/app

# Build macOS binary from Linux
GOOS=darwin GOARCH=arm64 go build -o kpr ./cmd/app
```

Supported targets:
- `linux/amd64` - Most Linux servers
- `linux/arm64` - ARM servers, Raspberry Pi 4+
- `linux/arm` (ARMv7) - Raspberry Pi 3
- `darwin/amd64` - Intel Macs
- `darwin/arm64` - Apple Silicon Macs

## Build Flags

The build uses these ldflags:

```bash
-s -w                                           # Strip debug info (smaller binary)
-X nrtn.dev/catalyst/kpr/internal/web.Version=$VERSION
-X nrtn.dev/catalyst/kpr/internal/web.Commit=$COMMIT
-X nrtn.dev/catalyst/kpr/internal/web.BuildTime=$BUILD_TIME
```

This embeds version information in the binary.

## Docker Build (Optional)

For reproducible builds, you can use Docker:

```bash
# Build inside Docker (Linux binary)
docker run --rm -v "$PWD":/src -w /src golang:1.21 \
    go build -o kpr-linux-amd64 ./cmd/app
```

## Binary Size

Typical binary sizes (stripped):
- Linux amd64: ~8.5 MB
- Linux arm64: ~8.0 MB
- Darwin arm64: ~8.2 MB

These are static binaries with no runtime dependencies.

## Troubleshooting

### Build fails with "command not found"

Install Go 1.21+:
```bash
# macOS
brew install go

# Linux
wget https://go.dev/dl/go1.21.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
```

### Version shows "dev" instead of tag

Make sure you have git tags:
```bash
git tag -a v0.1.0 -m "Initial release"
```

### Cross-compilation fails

Check GOOS/GOARCH are valid:
```bash
go tool dist list | grep linux
```

### Binary too large

The binaries are already stripped (`-s -w` flags). To compress:
```bash
upx dist/kpr-linux-amd64  # Requires upx
```

## CI/CD Integration

### Forgejo Actions (Current)

Workflows in `.forgejo/workflows/`:

**CI** (`ci.yml`):
- Runs on every push/PR
- Lints, tests, builds all platforms
- Uses `just` for builds

**Release** (`release.yml`):
- Runs on git tags (`v*`)
- Uses GoReleaser with Forgejo support
- Publishes to Forgejo releases

To create a release:
```bash
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

See [.forgejo/workflows/README.md](.forgejo/workflows/README.md) for details.

### GitHub Actions (Alternative)

```yaml
name: Release
on:
  push:
    tags:
      - 'v*'
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.21'
      - uses: goreleaser/goreleaser-action@v5
        with:
          version: latest
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```
