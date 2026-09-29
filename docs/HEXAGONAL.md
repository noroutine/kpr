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

## Where it leaked (diagnosis at start; status in Work below)

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
keeper/ gc/             use cases (pure orchestration, ports in)
sweep/                   pass loop (use case where it sits)
store/ registry/ otel/    outbound adapters
cli/ web/ app/            driving adapters (parse, call, render)
cmd/app                  the only wiring
```

## Work (status per step)

- [x] Step 1 — registry ports. `sweep.Registry` (delete),
  `keeper.CatalogSource` (was `cli.cataloger`), `keeper.Prober`
  (was `cli.prober` + web's inline port). Split after review into
  1-review: 12 sweep tests off the loopback server onto the stub,
  mid-pass failure test made map-order independent, one HTTP
  integration test kept. Gap left: `sweep` still imports `registry`
  for `Outcome*` — type decoupled, package arrow isn't; moves with
  a second backend or not at all.
- [x] Step 2 — keeper use cases, split 2a–2d: evaluation, `Reap`
  (mark-behind-armed), `FetchStatus` (one counter implementation),
  Plan (list/discard/add/remove). e2e imports `keeper`, never
  `cli`; `web` formats, `cli` prints. Topped up (review): direct
  `ListPlan` test, zero-branch messages, failing-store error paths.
- [~] Step 4 — gc use case, partial, started early on Oleksii's
  call (biggest use-case-in-adapter left, and the port-design
  lesson lives here):
  - [x] gc-1 sentinel: `ProbeRegistry`, `Mode`, `ProbeRepo` with
    tests; `cli.runGC` and e2e drive it from `gc`.
  - [ ] gc-2 proofs: `registryStoreRoot`, `storeLayout`,
    `sameStoreUpload`, `sameStoreTagLink`, `firstDigestRow` (+
    readiness gates) move with their tests. Pure groundwork, no
    ports yet.
  - [ ] gc-3 collector: `runCollector`, `GCEvent` stream,
    `collectorCommand` seam move; `Collector` port cut
    (production adapter shells the stock binary, tests keep the
    shell stub).
  - [ ] gc-4 orchestration: `runGC` + `GCOptions` move behind
    `Probe` (sentinel) and `Locker` (named lock) ports; `cli`
    keeps flags, wiring, and `renderGCEvent`.
  - Later, on the gc path (not the hexagon): Oleksii's read-sentinel
    same-store proof idea (`kpr-sentinel:latest`, API digest vs
    link-file revision) — parked in `docs/BACKFILL.md`, lands here
    when backfill unparks.
- [ ] Step 3 — narrow store interfaces. Deferred: no consumer has
  abused the fat port yet, and the lock collapse already removed
  the clearest duplication. Waits for evidence.
- [ ] Step 5 — single composition root. Deferred: janitorial, least
  learning per line; worth doing once, not now.
- Extra (review-suggested, taken): named lock port over
  `LockKey`/`GCLockKey` — twin contract tests became one plus a
  cross-lock independence check. Taken because the duplication was
  exact (same shape, same tests twice), not speculative.

## Experiment rules

- One slice at a time, TDD, pipeline green, coverage > 90%.
- No `Clock` port, no policy engine, no gRPC/IDL, no framework.
- `keeper` naming: `keeper` (repo vocabulary), not `usecase`/`service`.
- Steps 3+5 stay gated: each needs a "does it pay?" verdict, not
  auto-approval. Measure: lines deleted, fakes deleted, duplicated
  strings gone.

## Open questions

- Does `sweep` stay a use case package itself, or does the sweeper
  move under `keeper/` too? Lean: `sweep` keeps the pass loop,
  consumes the `Registry` port, no move.
- Per-consumer store interfaces: small interfaces at the use-case
  site (`keeper` defines what it needs) vs splitting `Store`.
  Lean: former, `Store` stays the implementation.
- `gc` ports: `Probe`/`Collector`/`Locker` as three tiny interfaces
  in the `gc` package, adapters in `cli` today. Extraction started
  early (sentinel first, gc-1); ports land as the orchestration
  moves.

## Verdict on Claude's outline

70%-hexagonal claim held against the code read at the time (gc was
indeed ~720 lines of use case in `cli`). Steps 1+2 were the right
first cut; gc extraction started early on evidence (biggest
use-case-in-adapter left). Narrow store ports still wait, as
proposed. `Clock` correctly dropped.
