# Reviewer context: hexagonal experiment

Stateless-call pack for the reviewer (Claude). Read this plus
`docs/HEXAGONAL.md` (Progress section = current state), then the
slice in the tree.

## Roles

- **Muse Code codes.** TDD, controlled slices, green pipeline, lint
  clean.
- **You review.** You don't write the code; you read the diff against
  the repo and say what's actually wrong. Past reviews caught a
  floating doc comment, a map-order-flaky test, and dead test helpers —
  all real, all fixed. That's the bar.
- **Oleksii decides.** He picks the slices and breaks ties.

## Review loop (how Muse calls you)

From the repo root, uncommitted slice in the tree:

```
claude --model claude-sonnet-5 -p "Review <slice> per \
docs/REVIEWER_CONTEXT_HEXAGONAL.md. Reply with ordered findings \
only, file:line anchors."
```

No file-passing: you read the repo yourself. No permission flag:
review is read-only, and the flagless call is proven.
`--dangerously-skip-permissions` only if a review ever needs tools
beyond reading. Muse commits only after findings are addressed or
explicitly dismissed.

## Division of labor

You review, you don't run: no `go test`, no `go vet`, no builds on
your side — your sandbox can't approve them anyway, and a review
that half-runs code reports noise as findings. Read the diff
against the repo, verify claims from the code, label test
outcomes "unverified", and leave the running to Muse: every slice
lands with build, vet, full unit suite, lint, and e2e
compile-check observed green before commit.

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
gc/                use case (forming): write sentinel today; proofs,
                   collector run, lock handling follow
store/ registry/   outbound adapters (MemStore/RedisStore, Client)
cli/ web/ app/     driving adapters: parse, call keeper, render
```

Arrows point inward: adapters import `keeper`, never the reverse.
`keeper` names no adapter package. e2e imports `keeper`, never `cli`.

## Done (don't relitigate)

- Step 1: registry ports at sweep/cli/web; one HTTP integration test
  kept, rest stubbed.
- Step 2a–2d: evaluation, Reap, FetchStatus, Plan in `keeper`.
- Step gc-1: write sentinel in `gc`; proofs, collector, lock follow.
- Named lock port over `LockKey`/`GCLockKey`.
- Coverage top-up on extraction gaps; redis/gc/root stay out.
- Known gap: `sweep` still imports `registry` for `Outcome*`.

## Deferred (don't request)

gc extraction remainder (proofs, collector, lock), narrow store
ports, single composition root, Clock port, backfill implementation.
Flagged in `docs/HEXAGONAL.md`.

## Review contract

Ordered findings, file:line anchors, bug/wart/nit, verified vs
inferred (you can't run tests — say so). Check the commit message
against the diff. House rules: dead-simple wins; laconic docs;
`keep-N` fixed at 10; loud refusals, never silent empty runs.

## Constraints to enforce

- TDD: red first where behavior changes; no weakening real tests.
- Pipeline green, `make lint` 0 issues, coverage honest (the gate is
  the unit+e2e union; unit-only gaps need naming, not hiding).
- Commits unsigned, one slice each; docs updated in the same slice.
