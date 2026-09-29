# Hexagonal experiment

Goal: test whether hexagonal layering makes kpr simpler to change,
without adding ceremony. Dead-simple rule wins: if a step does not
remove a fake HTTP server, a duplicated counter, or a scattered
wiring block, we skip it.

## Where we are

Already hexagonal-ish, without trying:

- `policy` is the core. `Row` + `Select*` are pure over
  `(rows, catalogs, now)`. `store` and `sweep` import `policy`,
  so dependencies point inward.
- `store.Store` is a proper outbound port. `RedisStore` + `MemStore`
  behind it, `storetest/contract.go` pins both to one contract.
- Time needs no port. `now` arrives as an argument, `Sweeper.Now`
  is the seam. No `Clock` interface.

## Where it leaks

1. `registry` has no port. `sweep.Sweeper`, `cli.EvaluatePolicy(s)`,
   `fetchCatalogs`, `runStatus`, and `web.Server` all hold
   `*registry.Client` concretely. Sweep tests need an HTTP server
   to fake the registry.
2. Use cases live inside driving adapters. `cli` owns `runReap`,
   `runPlan`, `runStatus`, `EvaluatePolicy(s)`; `web.keeperSnapshot`
   recomputes the same outcome counters as `cli.runStatus` with bare
   `"deleted"` / `"planned"` strings in both places. E2E imports
   `cli` to reach the use case.
3. `Store` is four ports in one: rows, run state (`Current` + activity
   ring), sweep lock, GC lock. One fat interface, every consumer
   sees everything.
4. `gc` (~720 lines across `cli/gc.go` + `cli/gc_run.go`) is a use case
   hiding in `cli`: sentinel probe + subprocess runner + lock
   orchestration, wired inline in cobra `RunE`.
5. Composition root is scattered. Every cobra `RunE` rebuilds config,
   `OpenStore`, `registry.NewClient` itself. `cmd/app` does not own
   the wiring.

Inbound bypass (stays): the mark is the interface. The redis hash is
a public inbound surface that bypasses use cases by design; the
sweeper TTL floor (never wipe before promise elapses) guards it, and
`storetest/contract.go` pins the key layout.

## Target (only if each step pays)

```
policy/                  core (unchanged)
keeper/  sweep-use?/ gc/  use cases (pure orchestration, ports in)
store/ registry/ otel/    outbound adapters
cli/ web/ app/            driving adapters (parse, call, render)
cmd/app                  the only wiring
```

Concrete moves, in order:

1. `Registry` interface next to its consumers. `sweep` needs
   `DeleteManifest`, reap needs `Catalog`, console needs `Reachable`.
   ~10 lines, kills the HTTP server in sweep tests.
2. `keeper` use cases out of `cli`/`web`: Reap, Plan
   (add/remove/discard), Status. One counter implementation, e2e
   stops importing `cli`.
3. Narrow store interfaces per consumer (rows / run-state / locks).
   Only when step 2 shows a consumer abusing the fat port.
4. `gc` use case + ports (`Probe`, `Collector`, `Locker`) out of
   `cli`. Only if `gc` keeps growing past backfill/sentinel work.
5. Single composition root in `cmd/app`. Each `RunE` stops rewiring.

## Progress

- Step 1 done: `sweep.Registry` (delete port), `cli` cataloger +
  prober (one method each — no consumer needed both), `web.Server`
  reachability port. Stub tests per consumer, no new loopback
  servers; the stubs also carry the failure paths (mid-pass error
  keeps the row due, held untracks). Full unit suite green, e2e
  compiles (docker run deferred to CI).
- Known gap (review): `sweep` still imports `registry` for the
  `Outcome*` delete vocabulary, and the stub tests reference it too —
  the type is decoupled, the package arrow isn't. Moving the
  vocabulary waits for a second backend or the gc extraction.
- Step 1b done (review): the mid-pass failure test fails the first
  call whatever the ref (map-order independent); 12 sweep tests moved
  off the loopback server onto the stub, the duplicated HTTP held
  test deleted. One integration test
  (`TestArmedPassDeletesAndResolves`) keeps the real client, so port
  and adapter stay proven together.
- Locks collapsed (review): one named-lock port
  (`AcquireLock(ctx, name, ttl)` / `ReleaseLock(ctx, name)`) over the
  existing `LockKey` / `GCLockKey`; the twin contract tests became one
  (plus a cross-lock independence check), redis was already keyed
  underneath.
- Step 2a done: `keeper` package owns evaluation (`EvaluatePolicy(s)`
  + `CatalogSource` port); `cli` calls it, e2e imports it instead of
  `cli`, and the two pure-evaluation tests moved to the use case's
  address. `cli/keeper.go` no longer imports `policy`.
- Step 2b done: `keeper.Reap` (evaluate + mark-behind-`armed`);
  `cli.runReap` is printing-only, e2e `ReapArmed` drives the use case
  instead of its own mark loop, refusal + armed/unarmed pinned in
  `keeper` tests.
- Step 2c done: `keeper.FetchStatus` (one counting implementation +
  `Prober` port); `cli.runStatus` fails on it, `web.keeperSnapshot`
  formats it. The duplicated outcome switch is gone; `web/server.go`
  no longer names a registry type.
- Step 2d done: `keeper` owns the plan (`ListPlan`, `DiscardPlan`,
  `AddPlan`, `RemovePlan` + `ManualReason`); `cli` renders counts and
  the JSON view. Behavior tests moved to the use case, message tests
  stayed in the adapter. `keeper.go`/`planedit.go` no longer import
  `policy`; only the deferred `gc.go` still does (Row type).
- Coverage top-up (review): direct `ListPlan` test (sort contract +
  dead-store refusal), zero-branch message tests in `cli`, failing
  fakes for `DiscardPlan`/`ListPlan` error paths. Extraction gaps
  closed; redis/gc/root stay environmental and out of scope.

## Experiment rules

- One slice at a time, TDD, pipeline green, coverage > 90%.
- No `Clock` port, no policy engine, no gRPC/IDL, no framework.
- `keeper` naming: `keeper` (repo vocabulary), not `usecase`/`service`.
- Stop after step 2 unless 3+4 pull their weight. Measure: lines
  deleted, fakes deleted, duplicated strings gone.

## Open questions

- Does `sweep` stay a use case package itself, or does the sweeper
  move under `keeper/` too? Lean: `sweep` keeps the pass loop,
  consumes the `Registry` port, no move.
- Per-consumer store interfaces: small interfaces at the use-case
  site (`keeper` defines what it needs) vs splitting `Store`.
  Lean: former, `Store` stays the implementation.
- `gc` ports: `Probe`/`Collector`/`Locker` as three tiny interfaces
  in the `gc` package, adapters in `cli` today. Defer until step 4.

## Verdict on Claude's outline

70%-hexagonal claim holds against the code read so far (receiver,
`cli/gc*.go`, config not re-read by Claude — verified above: gc is
indeed ~720 lines of use case in `cli`). Steps 1+2 are the right
first cut. Narrow store ports and gc extraction wait, as proposed.
`Clock` correctly dropped.
