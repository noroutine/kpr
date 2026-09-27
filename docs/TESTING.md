# Testing Guide

Unit tests today; this is also where integration test setup/notes will
land once the registry/redis-backed work starts (see `docs/goals.md`).

## Quick Start

```bash
just test              # or: make test
just coverage            # or: make coverage
just coverage-report     # reprint the last coverage table, no re-run
```

All three go through [`gotestsum`](https://github.com/gotestyourself/gotestsum)
for a dense per-package pass/fail summary when it's installed
(`make install-prereqs`, or `go install gotest.tools/gotestsum@latest`
for just that one), falling back to plain `go test -v` otherwise.
`-race` is always on. `coverage` additionally
writes `coverage.out` and renders `coverage.html`
(`open coverage.html` for the browsable per-line view).

## Test Tools

You can use either **justfile** or **Makefile** — CI standardizes on
`make`; `just` is for local development. Every recipe exists in both
files; keep them in sync when adding a new one.

### Using just

```bash
just test
just coverage
just coverage-report
just bench                # benchmarks only, no tests
just test-linux           # race+coverage suite inside a Linux container
just test-linux-verbose   # same, with -v
just test-linux-repeat n=50   # repeat N times (default 20) to chase a flake
just check                # fmt-check + vet + lint + test
```

### Using make

```bash
make test
make coverage
make coverage-report
make bench
make test-linux
make test-linux-verbose
make test-linux-repeat N=50
make check
```

`test-linux*` run inside a `golang:1.27` Docker container — for
Linux/filesystem-only flakes, not CPU-arch-specific ones. Module
downloads and build cache persist between runs in named Docker volumes
(`kpr-linux-test-gomod`, `kpr-linux-test-gobuild`) so repeat runs are
fast; the first run per volume pays the full download/compile cost. The
bind-mounted repo files are written by the container's root user, but
only `coverage.out` lands back in the tree (gitignored) — no source
files are touched.

## Coverage

`coverage` writes `coverage.out` (statement-based — Go has no
branch-coverage mode) and `coverage.html`. The terminal table
(`go tool cover -func`) is the report of record; CI prints it to the job
log (the runners have no artifact service, so nothing is uploaded).
Coverage is informational — no percentage gate.

## CI Integration

Workflows live in `.forgejo/workflows/` and call `make` targets rather
than `go test` directly, so a CI failure gets the same dense `gotestsum`
summary a local run does instead of scrolling `-v` output.

**CI** (`ci.yml`) — runs `make coverage` (via `gotestsum`) on every
push/PR to `master`, alongside format check, `go vet`, and lint
(`golangci-lint` with the pinned set in `.golangci.yml` when installed,
`go vet` fallback otherwise). `make test-docker` builds the packaged
image and probes both servers' endpoints from inside the container.

**Release** (same file, `release` job) — rebuilds all platforms on a tag
push, pushes the multiplatform image, and attaches the binaries to the
Forgejo release. Publishing happens only on tags; ordinary pushes only
build and verify.

## Mutation testing

Statement coverage can't tell a test that asserts nothing from one that
asserts the right thing. `mutation` (gremlins, pinned `v0.6.0`, installed
via `go install` so it stays out of `go.mod`) mutates covered code and
checks the suite catches it:

```bash
make mutation-dry   # list candidates without running anything
make mutation       # full run: kills or explains every mutant
```

The run gates on `--threshold-efficacy=90` (killed over killed+lived)
and `--threshold-mcover=85` (covered mutants over all mutants), exiting
nonzero below either. Both sit below the achieved baseline with room to
tighten — raise them, don't lower them, when the suite improves.

Two tunables, both learned the hard way:

- `--timeout-coefficient=100`: gremlins derives each mutant's timeout
  from its covering tests' own milliseconds; at the default coefficient
  of 3 every mutant spuriously `TIMED OUT` because a test binary can't
  start and serve in that budget.
- `--workers=4` (override with `WORKERS=...` on make,
  `just mutation workers=...`): caps parallelism so hungry test runs
  don't starve each other into timeouts. If `TIMED OUT` climbs with no
  code change, lower workers before touching anything else.

A full run takes minutes and is deliberately **not** part of `check` or
per-push CI — run it periodically and before significant test changes,
preferably on a quiet machine: contended CPUs inflate `TIMED OUT` (which
doesn't fail the gates, just slows the run). The manual `Mutation`
Forgejo workflow (`workflow_dispatch`) runs the same `make mutation`
gate on demand.

## Integration tests

Not started yet. Notes on scope, fixtures (registry + redis from
`docker-compose.yml`), and how they fit alongside the existing unit
suite will land here once that work begins. Two rules already apply:

- Tests must skip gracefully when the fixtures are not running.
- Every fixture address is built from an env-exported host — never
  hardcoded `localhost` (inside CI job containers the fixtures are
  siblings whose published ports live on the host, so container
  localhost never works there).
