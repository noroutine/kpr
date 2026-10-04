# Testing Guide

Unit suite plus container-backed end-to-end scenarios (`test/e2e`,
`e2e` build tag) plus periodic mutation runs. What each layer
covers, and the gates each must pass.

## Contents

- [Quick Start](#quick-start)
- [Test Tools](#test-tools)
- [Coverage](#coverage)
- [CI Integration](#ci-integration)
- [Mutation testing](#mutation-testing)
- [End-to-end scenarios (`test/e2e`, `e2e` build tag)](#end-to-end-scenarios-teste2e-e2e-build-tag)

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

Unit-only coverage excludes the redis happy paths on purpose (they
live in e2e now): `coverage-e2e` runs both suites with
`-coverpkg` over product packages and unions them into
`coverage-e2e.out`, so the harness files themselves never count
against the total. The union — not the unit number — is the bar
that must stay above 90%.

`internal/storetest` is excluded from the unit roll-up in the
Makefile (filtered out of the profile before reporting): it is
shared contract scaffolding imported by the store suites —
exercised only under docker-backed runs — not product code,
same reason `*_test.go` files never count. Filtering it moves
the unit total from ~80% to ~86% with zero product code
touched. `coverage-e2e` keeps it: under that profile the
helpers genuinely execute.

Named residue stays below 80% on purpose — error returns that
cannot fire without contorting the test: `json.Marshal` on
plain structs (store `SetIdentity`/`SetCurrent`/`writeRow`,
lock-claim marshal), `os.Hostname` (`holder`), `uuid.NewV7`
randomness (`NewGen`), `fs.Sub` on embedded static
(`GetStaticFS`), and the lock-acquire leftovers (claim
truncate/write, stale-fd close). Forcing these means
monkeypatching the stdlib or faulting disks; a test that
fakes the failure proves the fake, not the code.

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

**Future: permission testing under root.** CI runs in an `act`
container as root, and root ignores permission bits — so
chmod-staged refusal tests skip there (`os.Geteuid() == 0`, the
house convention), and `RLIMIT_FSIZE` sabotage must dodge the
harness's own log writes (process-global limit: ceiling above
framework appends, or a child process holding the limit). That
leaves the refusal paths unexercised exactly where the project
claims coverage. Options, undecided: a non-root CI user, a
dedicated permission stage, or root-proof sabotage (read-only
bind mounts). Until then, skips stay loud and named.

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

Not every survivor is a missing test. Four kinds are expected, and
reaching for a test to kill them is wasted work:

- **Timing mutants** — a changed duration no assertion can observe.
- **Provable equivalents** — the mutation yields identical behavior.
  NOTE these at the site so the next run doesn't re-litigate them.
- **Dead-server error convergence** — several failure paths produce
  the same error, so swapping between them is invisible.
- **Live-redis branches** — only reachable under `-tags e2e`.

Anything outside those four is killable: write the focused test.

Gremlins mislabels mutants on statement-continuation lines as NOT
COVERED and skips them: a multi-line `if` whose condition sits on
a later line, a `case` sharing its operator line. The label is a
lead, not a verdict — check the real profile
(`go test -coverprofile`) for execution, and hand-flip the line
when the two disagree (a flip the suite fails is killed,
whatever the label says). TIMED OUT is a kill by another name:
the mutant hangs (ranging a nil channel, an undrained pipe),
which no passing run can do.

Three more attribution artifacts, all proven by hand-flips that
the suite catches loudly:

- **`const` declarations never cover.** Coverprofiles emit no
  blocks for `const` lines, so every mutant on one reads NOT
  COVERED even when tests pin the value. Check for literal pins
  instead: reason strings rendering the tuning (`partial:older
  than 24h0m0s`, `untagged:past grace 168h0m0s`), staged
  literals around the window (3-day/8-day proofs, 2h rows),
  wire text asserting the constant (the manifest Accept list).
  When the value renders nowhere observable, ±1 steps are
  accepted tuning (NOTE'd at the site); digit-scale typos stay
  caught by the order pins.
- **Walk-closure `}); err` lines.** `filepath.WalkDir` callbacks
  attribute to the closing line; the guard bodies inside execute
  (a flip collapses classification and the layout tests go red).
- **Import lines.** A mutant attributed to an import block is
  noise — there is no expression there to mutate.

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

Test scaffolding is excluded from candidacy (`--exclude-files` in
the `mutation` target: the e2e harness files plus
`internal/storetest/contract.go`): mutants in test support measure
nothing — they break tests trivially or live only because the
docker suites don't run. Same rationale as the coverage filter,
and it keeps mutator coverage a meaningful gate instead of a
scaffolding census.

Accepted survivors (equivalent or untestable-by-construction — every
one earned, none by neglect):

- `internal/policy/policy.go` clamp guards: the boundary inputs evaluate
  to exactly `MaxTTL` on both sides, so the mutants are provably
  equivalent — no test can distinguish them.
- Sort-comparator boundaries (`cli/keeper.go`, `keeper/keeper.go`,
  `keeper/plan.go`): equal keys sort identically under `<=`, so the
  only distinguishing inputs have indistinguishable outputs.
- Activity-ring trim (`store/file.go`, `store/mem.go`): trimming an
  exactly-cap ring is identity, NOTE'd at the site.
- Boundary-at-value equivalents (each NOTE'd at the site): clock
  zero-negation (negating zero is identity), lineage tag tie-break
  (tags unique per row set), `match.go` `:tag` refusal (identical
  message either branch), policy bound-1 (719 still clamps
  everything ≥720h identically).
- Timing constants (`registry.go:35`, `gc/proof.go` cache timeouts,
  `gc/sentinel.go` probe timeout): changed timeouts don't change
  observable behavior.
- Dead-server error convergence (`store/redis.go` Record/MarkDue/
  UnmarkDue HGet guards): with no server HGet and the follow-on
  HSet fail identically, so the guard mutants converge — except
  MarkDue's, whose error identity is pinned by the root-error test.
- Live-redis branches that only die under `-tags e2e` (GetCurrent
  empty, activity LTrim/LRange bounds): the e2e contract asserts
  the exact values; the unit run never executes them.
- `otel/telemetry.go:95` (`initMetrics` error): needs a broken global
  OTel SDK — same untestable family as the Once-guarded Warn survivor.
- Tuning windows (each NOTE'd at the site): `ProofStaleAfter`
  (3-day/8-day pins days-against-hours), `uploadStaleAge`
  (fresh veto and 48h residue pin the order), repaint
  `liveInterval` (collapse disables the throttle). Only the
  collapse is observable — it dies on the order pins, each
  proven by hand-flip; finer steps change nothing a test
  should observe. Rendered tunings need no NOTE: the dashboard
  pins `tolerance 30s`, the reasons pin `24h0m0s`/`168h0m0s`.
- Bounds, not budgets: sweep `LockTTL` (minute steps
  unobservable — same family as gc's `lockTTL`/`holdLease`);
  collapse to zero dies on the positivity pin (redis PX).
- Repaint throttle (`cli/progress.go` `liveInterval`): the
  throttle test pins paint-then-silence; no test measures 300ms.
- Layout-only shapes (`gc/husk.go` walk guards): `_manifests`
  is always a dir in registry-produced layouts, so only
  hand-planted trees distinguish the mutants.

## End-to-end scenarios (`test/e2e`, `e2e` build tag)

Behavioral scenarios run the keeper pipeline against real containers
via [testcontainers-go](https://github.com/testcontainers/testcontainers-go):
a password-protected `redis:8-alpine` (kpr rows on DB 4, mirroring
compose) plus a delete-enabled `registry:3`. Image pushes go through
[go-containerregistry](https://github.com/google/go-containerregistry)
(`remote.Write` — no docker daemon involved beyond the containers
themselves). A small scenario DSL (`scenario.go`: `Push`, `ReapArmed`,
`SweepArmed`, `Expect*`) drives the same `EvaluatePolicies`,
`MarkDue`, and `Sweeper.RunPass` the CLI runs — one policy
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
tag-fallback, untagged-after-grace, a seven-client push matrix
(ggcr, crane, docker daemon, regclient, oras typed artifacts,
skopeo copy, podman), multi-arch index sweeps, referrer
precision (an expired signature artifact sweeps while its subject
stays listed and fetchable), the sentinel round-trip
(write layout → API read → repoint serves fresh) and keep-N over
generations (twelve mints → two marked → swept, ten survivors,
floater serving newest), the lineage verdict matrix live
(establish on silence, foreign refuses then `adopt` heals,
rollback heals via `adopt --gen`, sweeper skips foreign
dry-run), and the lock gate (`unlock` opens, unshared refuses).
The edge fence (`test/e2e/edge_test.go`, same harness): locked
manifest PUT/DELETE refuse 423 naming the remedy — even for tags
that don't exist — while reads stay byte-identical to direct and
blob uploads pass; unlocking forwards PUT (201), DELETE (202),
and reads; an armed collect holds a concurrent PUT (held, then
201 after release) while a preview never engages the fence.
Clock checks run against a hermetic `httptest` time server —
never the real network.

Client homes: Go-library clients (ggcr, crane, regclient) run
in-process. Binary-only clients (oras, skopeo) run in one toolbox
container (`testdata/toolbox`, built once per package run by
TestMain) that reaches fixtures by container IP — no host binary
needed, no port forwards involved. The docker CLI must stay
host-bound (pushes originate inside the daemon): it skips on Docker
Desktop (macOS/Windows), where the daemon's localhost cannot reach
fixture ports — native Linux daemons, CI included, run it. Podman
is daemonless so it shares the test's viewpoint; it runs wherever
the binary is present and ready, and skips otherwise.

Not yet covered: the receiver-notification path (scenarios record the
row the receiver would track; a serve-booting scenario asserting
push → notification → row is the next slice), the registry
blobdescriptor cache on shared redis, and offline GC.
