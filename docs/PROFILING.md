# Profiling kpr <!-- omit in toc -->

How to find what a running kpr is actually doing: capture a
profile, read it, and — the lesson this document exists to
teach — distrust the first story the numbers tell until a
second measurement confirms it.

## Contents <!-- omit in toc -->

- [The setup](#the-setup)
- [Capturing](#capturing)
- [Reading a CPU profile](#reading-a-cpu-profile)
- [Goroutines, heap, and the rest](#goroutines-heap-and-the-rest)
- [Benchmarks](#benchmarks)
- [Rules that earned their place](#rules-that-earned-their-place)

## The setup

`serve` exposes `net/http/pprof` when `KPR_PPROF_ADDR` is
set, e.g. `KPR_PPROF_ADDR=127.0.0.1:6060`. Empty (the
default) leaves it off. Two guardrails, both deliberate:

- Only loopback binds. Anything else refuses the boot —
  pprof can dump heap contents, so the endpoints never face
  a network. There is no flag, env only, like the backend
  secret.
- Nothing ships on by default. Profiling is an explicit
  opt-in per run, never a standing port.

Verify it is up before loading the system: the boot log names
it (`Starting pprof on 127.0.0.1:6060`), and
`curl http://127.0.0.1:6060/debug/pprof/` lists the available
profiles.

## Capturing

Against a loaded serve (your own load harness, or production
traffic), run from the repo root:

```sh
just pprof-cpu                        # 30s CPU, top callers
just pprof-cpu addr=127.0.0.1:6060 seconds=60
just pprof-heap                       # what it holds, not what it burns
```

The Makefile carries the same targets
(`make pprof-cpu PPROF_ADDR=... PPROF_SECONDS=...`). `go tool
pprof` fetches and symbolizes straight from the URL — no
local binary needed. Profiles also save under `~/pprof` for
later (`go tool pprof -http=:8080 ~/pprof/<file>` opens the
interactive browser: top, graph, flame, source).

For one-off endpoints without a recipe:

```sh
curl -o /tmp/gor.txt 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1'
```

## Reading a CPU profile

`go tool pprof -top` shows where sampled time went. Read it
in this order:

1. **Total samples vs duration.** `Total samples = 9.50s
   (31.50%)` over 30s means the process burned a third of one
   core. If the number is small while throughput lags, the
   bottleneck is off-CPU (waits the sampler never sees) —
   stop reading the top and go look at goroutines and block
   behavior instead.
2. **flat vs cum.** `flat` is time in the function itself;
   `cum` includes everything it called. Your code rarely tops
   flat — the runtime and syscalls do. Follow `cum` down
   through `net/http` into your handlers (e.g.
   `internal/app` or `internal/console`) to find whose work
   the syscalls serve.
3. **`go tool pprof -traces`.** When one leaf (e.g.
   `syscall.rawsyscalln`) dominates, traces attribute it:
   each sample prints its full stack, so `open`/`write`/
   `fsync` samples sort themselves by caller.
4. **macOS names lie a little.** `os.File.Sync` samples show
   as `runtime.fcntl`, never "fsync" — Go's Darwin build
   implements `Sync` as `fcntl(FULLFSYNC)` (see
   `internal/poll/fd_fsync_darwin.go`, Go issue #26650).
   Searching the top for "fsync" and finding nothing does
   not mean nothing syncs.

Heap (`inuse_space`): what the process retains — look for
growth across captures, not single snapshots. Goroutines
(`debug=1` text): what every thread is doing right now —
aggregate identical stacks to see where accepts and
requests pile up.

## Benchmarks

Profiles point at a hot path; a benchmark is how you hold
that path still and price a change to it. `make bench` (or
`just bench`) already runs `go test -bench` — the tree just
ships no benchmarks yet, on purpose: a benchmark without a
question is decoration. Add one in the package whose cost
matters, when a profile (or a code review) names the cost:

```go
// BenchmarkX prices one durable operation end to end. Solo
// ns/op is the floor no worker count can beat.
func BenchmarkX(b *testing.B) {
	for i := 0; i < b.N; i++ {
		// one unit of the hot-path work
	}
}
```

```sh
go test -run=NONE -bench=X -benchmem ./internal/<pkg>/
```

Split a multi-phase operation into `b.Run` sub-benchmarks
per phase when its total can disagree with the sum of its
parts — the split names the phase instead of blaming "the
disk".

Costs differ per OS: a number taken on the dev laptop says
nothing about the Linux box that serves. Measure on the
deploy target before quoting. Recipe (no toolchain needed
on the target):

```sh
GOOS=linux GOARCH=amd64 go test -c -o /tmp/kpr-linux.test ./internal/<pkg>/
scp /tmp/kpr-linux.test target:~/bench/kpr.test
ssh target 'cd ~/bench && TMPDIR=~/bench \
  ./kpr.test -test.run=NONE -test.bench=X -test.benchtime=300x'
```

Two cautions. First, keep durability runs off tmpfs
(`/tmp` on many boxes is tmpfs — fsync there measures
nothing). Second, medians wander run to run (page cache,
drive-internal batching), so quote the conservative run and
re-baseline on the real deploy box before promising
anything there.

## Rules that earned their place

1. **Quiet before quoting.** Kill the load, wait for IO to
   settle, then measure. A number taken under load describes
   the load.
2. **Name the syscall, not the wrapper.** `File.Sync` is
   three different costs on three OSes. When two languages
   (or two spellings of a call) disagree on the "same" cost,
   read both runtimes before theorizing.
3. **A/B in the same minute.** Interleave candidate and
   control rather than comparing numbers taken an hour
   apart — the machine drifts under you (Spotlight,
   throttling, yesterday's benchmark load).
4. **Flat lies, cum tries.** Follow cumulative stacks to
   your code; treat unattributed leaves as directions, not
   verdicts.
5. **Small samples convict.** `Total samples = 220ms` over a
   2.4s wall means 90% happened off-profile — the profile
   describes the minority. Say so before concluding.
