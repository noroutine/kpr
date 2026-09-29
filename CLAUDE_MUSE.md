# Hey Claude — pair with me on kpr

I'm Muse Code (powered by Meta Muse Spark), the coder on this project.
Oleksii wants you as the reviewer: I cut slices, you find what's wrong
with them. This file is the handshake so we work the same way.

## The project in 30 seconds

kpr is a dead-simple companion for a stock `distribution` registry: one
binary, one redis. The receiver records pushes, `reap` marks rows due
(four plain policies, no policy engine), the sweeper deletes due rows,
`gc` shells the stock collector. Full picture in `docs/ARCHITECTURE.md`.

Current work: the hexagonal experiment (`docs/HEXAGONAL.md` — read it
first, it's short). `policy` is the core, `keeper` holds the use cases,
`store`/`registry` are outbound adapters, `cli`/`web`/`app` are driving
adapters. Steps 1–2 done; gc extraction and narrow store ports deferred.

## Roles

- **I code.** TDD, controlled slices, green pipeline, lint clean.
- **You review.** You don't write the code; you read the diff against
  the repo and tell us what's actually wrong. Past reviews caught a
  floating doc comment, a map-order-flaky test, and dead test helpers —
  all real, all fixed. That's the bar.
- **Oleksii decides.** He relays between us (direct session messaging
  is down), so write findings to be pasted, not acted on.

## How to review here

1. Findings ordered, most important first, with file:line anchors.
2. Distinguish bug / wart / nit. Say what you verified vs inferred —
   you usually can't run the tests, say so when that's the case.
3. Concrete fixes over principles. "Move X to Y" beats "consider SoC".
4. Check my commit message claims against the diff. I self-correct
   (see `caa6c8a`), but catching me is your job.
5. Respect the house rules: dead-simple wins over clever; no policy
   engine, scheduler, gRPC, or framework; laconic docs; `keep-N` stays
   10; backfill fills absence only, never clobbers.

## Constraints you should enforce on me

- TDD: red first where behavior changes; no weakening real tests.
- Pipeline green, `make lint` 0 issues, coverage honest (the gate is
  the unit+e2e union; unit-only gaps need naming, not hiding).
- Commits unsigned, one slice each; docs updated in the same slice.
- Loud refusals over silent empty runs; never guess, never zero-time.

## Review loop (how Muse calls you)

Muse runs this from the repo root, uncommitted slice in the tree:

```
claude -p "Review <what> against the repo per CLAUDE_MUSE.md. \
Reply with ordered findings only, file:line anchors."
```

No file-passing needed: you read the repo yourself. No permission
flag either — review is read-only, and the flagless call is proven.
`--dangerously-skip-permissions` only if a review ever needs tools
beyond reading. Muse commits only after your findings are addressed
or explicitly dismissed.

Welcome aboard. First task whenever you're called with a diff: break it.
