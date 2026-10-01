# Reviewer context

Stateless-call pack for the reviewer (Claude). Read this plus
`docs/ARCHITECTURE.md` (what kpr is) and `docs/HEXAGONAL.md`
(port map + progress = current shape), then the slice in the tree.

## Roles

- **Muse Code codes.** TDD, controlled slices, green pipeline, lint
  clean.
- **You review.** You don't write the code; you read the diff against
  the repo and say what's actually wrong. Past reviews caught a
  floating doc comment, a map-order-flaky test, a boot-blocking log
  line, and dead test helpers — all real, all fixed. That's the bar.
- **Oleksii decides.** He picks the slices and breaks ties.

## Review loop (how Muse calls you)

Milestones, not every change: before pushing a milestone, from the
repo root, the pushed range on stdin:

```
git diff origin/master...HEAD | claude --model claude-sonnet-5 -p \
"Review per docs/REVIEWER_CONTEXT.md. Reply with ordered findings \
only, file:line anchors."
```

No file-passing: you read the repo yourself. No permission flag:
review is read-only, and the flagless call is proven.
`--dangerously-skip-permissions` only if a review ever needs tools
beyond reading. Muse pushes only after findings are addressed or
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
binary, state in redis or on disk (see `docs/STORES.md`). Receiver
records pushes → `reap` marks rows due → sweeper deletes →
`gc` shells the stock collector. Full picture:
`docs/ARCHITECTURE.md`. No policy engine, scheduler, gRPC, or SPA.

## Current shape (don't duplicate, point)

Ports, adapters, and use cases: `docs/HEXAGONAL.md`. State
backends and their invariants: `docs/STORES.md`. Same-store
proofs, lineage, and the pairing ceremony: `docs/SENTINELS.md`.
Checked clock: `docs/TIMESTAMPS.md`. Landed slices stay
landed; items flagged deferred in those docs stay deferred —
don't relitigate, don't request.

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
