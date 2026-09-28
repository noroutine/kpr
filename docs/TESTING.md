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

Start a local redis before the run (`redis-server --port 6379`, or the
compose stack): without it the whole `RedisStore` contract skips and
`redis.go` lands in NOT COVERED, tanking the score for no reason.

Thresholds are advisory in gremlins v0.6.0: `--threshold-efficacy` /
`--threshold-mcover` are accepted but never fire (a scoped probe at
100/100 still exits 0), so read the tally yourself instead of trusting
the exit code.

Gremlins' NOT COVERED set is advisory, not ground truth: its coverage
pass occasionally misses lines the suite provably executes (verified
by hand-mutating `sweep.go:139/143` and `registry.go:90-95` — the tests
catch both loudly). Trust a failing hand-mutant over the label.

Accepted survivors (equivalent or untestable-by-construction — every
one earned, none by neglect):

- `policy.go` clamp guards (`59`, `77`): the boundary inputs evaluate
  to exactly `MaxTTL` on both sides, so the mutants are provably
  equivalent — no test can distinguish them.
- Sort-comparator boundaries (`web/keeper.go` plan order, `cli/keeper.go`
  plan/evaluate order): equal keys sort identically under `<=`, so the
  only distinguishing inputs have indistinguishable outputs.
- `store/mem.go` ring trim (`97`): trimming at-cap is a no-op either way.
- Timing constants (`registry.go:35`, `cli/keeper.go:199,242`,
  `sweep.go:30`): changed timeouts don't change observable behavior.
- `otel/telemetry.go:95` (`initMetrics` error): needs a broken global
  OTel SDK — same untestable family as the Once-guarded Warn survivor.

## End-to-end scenarios (`test/e2e`, `e2e` build tag)

Behavioral scenarios run the keeper pipeline against real containers
via [testcontainers-go](https://github.com/testcontainers/testcontainers-go):
a password-protected `redis:8-alpine` (kpr rows on DB 4, mirroring
compose) plus a delete-enabled `registry:3`. Image pushes go through
[go-containerregistry](https://github.com/google/go-containerregistry)
(`remote.Write` — no docker daemon involved beyond the containers
themselves). A small scenario DSL (`scenario.go`: `Push`, `ReapArmed`,
`SweepArmed`, `Expect*`) drives the same `EvaluatePolicies`,
`MarkDue`, and `Sweeper.RunPass` the CLI and serve run — one policy
path, never a copy. `PushedAt` is backdated instead of sleeping on a
clock, so scenarios stay fast and deterministic.

```bash
just e2e            # or: make e2e
go test -race -tags e2e ./test/e2e/ -count=1 -v   # the long form
```

Two rules carry over from the fixture days:

- Scenarios skip gracefully when no docker answers (`docker info`
  gate); a container that fails to start once docker answers is a
  real failure, never a skip.
- Every fixture address comes from the container runtime (mapped
  ports) — never hardcoded `localhost`.

Covered so far: TTL expiry, keep-N retention, stale-upload
tag-fallback, untagged-after-grace, a four-client push matrix
(ggcr, crane, docker daemon, regclient), and multi-arch index
sweeps. The docker subtest skips on Docker Desktop (macOS/Windows),
where the daemon's localhost cannot reach fixture ports — native
Linux daemons, CI included, run it.

Not yet covered: the receiver-notification path (scenarios record the
row the receiver would track; a serve-booting scenario asserting
push → notification → row is the next slice), the registry
blobdescriptor cache on shared redis, and offline GC.
