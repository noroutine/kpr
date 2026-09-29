# Hexagonal architecture

kpr is hexagonal: `policy` core, `keeper`/`gc` use cases,
`store`/`registry` outbound adapters, `cli`/`web` driving adapters.
It got there one paying slice at a time — the experiment concluded,
this is the shape now. Dead-simple rule still wins: any new port
has to remove a fake, a duplication, or a scattered wiring block,
or it doesn't get cut.

## Where we are

Already hexagonal-ish, without trying:

- `policy` is the core. `Row` + `Select*` are pure over
  `(rows, catalogs, now)`. `store` and `sweep` import `policy`,
  so dependencies point inward.
- `store.Store` is a proper outbound port. `RedisStore` + `MemStore`
  behind it, `storetest/contract.go` pins both to one contract.
- Time needs no port. `now` arrives as an argument, `Sweeper.Now`
  is the seam. No `Clock` interface.

## What leaked (all five closed)

1. `registry` had no port — `sweep.Sweeper`, `cli`, and `web` all
   held `*registry.Client` concretely, and sweep tests needed an
   HTTP server to fake the registry. Now `sweep.Registry`,
   `keeper.CatalogSource`, `keeper.Prober`; sweep tests run on a
   stub, one HTTP integration test kept.
2. Use cases lived inside driving adapters — `cli` owned reap, plan,
   status, evaluation, and `web` recomputed the same counters with
   duplicated strings. All moved to `keeper`; `web` formats,
   `cli` prints.
3. `Store` is four ports in one — kept fat deliberately (step 3:
   one package of callers per cluster, single implementation;
   narrowing would be ceremony). Clustering documented per-method.
4. `gc` (~720 lines) hid in `cli` — extracted to the `gc` package
   behind `Probe`/`Collector`/`Locker` (gc-1–gc-4).
5. Wiring was scattered across every cobra `RunE` — collapsed onto
   `cli.openDeps` (step 5).

Inbound bypass (stays): the mark is the interface. The redis hash is
a public inbound surface that bypasses use cases by design; the
sweeper TTL floor (never wipe before promise elapses) guards it, and
`storetest/contract.go` pins the key layout.

## Shape (each step paid, so all of it landed)

```
policy/                  core (unchanged)
keeper/ gc/             use cases (pure orchestration, ports in)
sweep/                   pass loop (use case where it sits)
store/ registry/ otel/    outbound adapters
cli/ web/                driving adapters (parse, call, render)
cli.openDeps             the composition root
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
  Plan (list/discard/add/remove). Behavior comes from `keeper`;
  only `OpenStore` wiring still comes from `cli` (step-5
  territory). `web` formats, `cli` prints. Topped up (review):
  direct `ListPlan` test, zero-branch messages, failing-store
  error paths.
- [x] Step 3 — narrow store interfaces: closed whole, by its own
  rule. 3a (marks) and 3b (run-state read) each have one package of
  callers but a single implementation — and MemStore tests already
  prove more than their stubs could. 3c (sweep `Locker` twin) has
  no consumer; `gc.Locker` keeps its own. 3d closed earlier. The
  clustering survives where it already lived: the per-method docs
  ("the reap interface", "the plan-discard interface"). Step output
  is megawisdom exhibits, not interfaces.
- [x] Step 4 — gc use case, done (started early on Oleksii's
  call: biggest use-case-in-adapter left, and the port-design
  lesson lives here):
  - [x] gc-1 sentinel: `ProbeRegistry`, `Mode`, `ProbeRepo` with
    tests; `cli.runGC` and e2e drive it from `gc`.
  - [x] gc-2 proofs: `StoreRoot`, `SameStoreUpload`,
    `SameStoreTagLink`, `FirstDigestRow` (+ readiness gates)
    moved with their tests. Pure groundwork, no ports yet.
  - [x] gc-3 collector: `RunCollector`, `GCEvent` stream,
    `collectorCommand` seam moved; `Collector` port cut
    (production shells the stock binary, tests keep the shell
    stub; consumed by the orchestration in gc-4).
  - [x] gc-4 orchestration: `Run` + `Options` moved behind
    `Probe` (sentinel), `Collector`, and `Locker` (named lock)
    ports; `cli` keeps flags, wiring, and `renderGCEvent`. A
    stub-port test drives the full pass with no network or binary.
- [x] Step 5 — single wiring point: `cli.openDeps` builds config,
  store, and registry client once per command (serve's long-lived
  `Run` keeps its own shape). Eight RunEs collapsed, fail-fast
  pinned by test. Not `cmd/app` injection — that would restructure
  every cobra var for no new substitution.
- Extra (review-suggested, taken): named lock port over
  `LockKey`/`GCLockKey` — twin contract tests became one plus a
  cross-lock independence check. Taken because the duplication was
  exact (same shape, same tests twice), not speculative.

## Standing rules

- One slice at a time, TDD, pipeline green, coverage > 90%.
- No `Clock` port, no policy engine, no gRPC/IDL, no framework.
- `keeper` naming: `keeper` (repo vocabulary), not `usecase`/`service`.
- New ports stay gated: each needs a "does it pay?" verdict, not
  auto-approval. Measure: lines deleted, fakes deleted, duplicated
  strings gone. Steps 3+5 passed through this gate and closed.

## Settled questions

- `sweep` stays a use case package itself: it keeps the pass loop
  and consumes the `Registry` port, no move under `keeper/`.
- Per-consumer store interfaces declined: `Store` stays the
  implementation, clustering documented per-method (step 3).
- `gc` ports landed as three tiny interfaces in the `gc` package
  (`Probe`/`Collector`/`Locker`), adapters in `cli` (gc-1–gc-4).

## How hexagonal is it

Behaviorally, fully: every outbound effect in the use cases goes
through a substitutable port (`sweep.Registry`,
`keeper.CatalogSource`/`Prober`, `gc.Probe`/`Collector`/`Locker`,
`store.Store` under a contract both adapters honor), and unit
tests prove it — no HTTP server, no binary, no redis needed.

Package-graph purists would find three arrows pointing the "wrong"
way: use-case signatures still name adapter packages for types —
`store.Store` + `store.GCLockKey` in `keeper`/`gc`, `registry.Outcome*`
in `sweep`. Those close with a second *differing* implementation
or not at all (megawisdom: a port needs a second implementation
that differs in a way someone uses). Until then, narrowing them
is ceremony with zero substitution payoff.

Deliberate non-textbook bits: the composition root is
`cli.openDeps`, not `cmd/app` injection (step 5 — restructuring
every cobra var buys nothing); the redis due-mark is a public
inbound surface bypassing use cases by design (guarded by the
sweeper TTL floor).

## Verdict on Claude's outline

70%-hexagonal claim held against the code read at the time (gc was
indeed ~720 lines of use case in `cli`). Steps 1+2 were the right
first cut; gc extraction started early on evidence (biggest
use-case-in-adapter left). Narrow store ports were declined by
their own rule (step 3). `Clock` correctly dropped.
