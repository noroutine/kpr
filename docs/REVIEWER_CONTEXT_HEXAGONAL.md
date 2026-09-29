# Reviewer context: hexagonal experiment

Stateless-call pack for the reviewer. Read this plus
`docs/HEXAGONAL.md` (Progress section = current state), then the
slice in the tree.

## What kpr is

Dead-simple companion for a stock `distribution` registry: one
binary, one redis. Receiver records pushes → `reap` marks rows due
→ sweeper deletes → `gc` shells the stock collector. Full picture:
`docs/ARCHITECTURE.md`. No policy engine, scheduler, gRPC, or SPA.

## Hexagon map (today)

```
policy/            core: Row + pure Select* (never touch)
keeper/            use cases: Evaluate, Reap, FetchStatus, Plan
                   ports: CatalogSource, Prober (+ sweep.Registry)
store/ registry/   outbound adapters (MemStore/RedisStore, Client)
cli/ web/ app/     driving adapters: parse, call keeper, render
```

Arrows point inward: adapters import `keeper`, never the reverse.
`keeper` names no adapter package. e2e imports `keeper`, never `cli`.

## Done (don't relitigate)

- Step 1: registry ports at sweep/cli/web; one HTTP integration test
  kept, rest stubbed.
- Step 2a–2d: evaluation, Reap, FetchStatus, Plan in `keeper`.
- Named lock port over `LockKey`/`GCLockKey`.
- Known gap: `sweep` still imports `registry` for `Outcome*`.

## Deferred (don't request)

gc extraction, narrow store ports, single composition root, Clock
port, backfill implementation. Flagged in `docs/HEXAGONAL.md`.

## Review contract

Ordered findings, file:line anchors, bug/wart/nit, verified vs
inferred (you can't run tests — say so). Check the commit message
against the diff. House rules: dead-simple wins; laconic docs;
`keep-N` fixed at 10; loud refusals, never silent empty runs.
