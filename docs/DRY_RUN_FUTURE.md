# Armed preview — future work

Converging every preview/armed pair onto one code path. Shipped
principles live in [DRY_RUN.md](DRY_RUN.md). ("Dry-run" below
means the user-facing default — no `--no-dry-run` passed; inside
the code it is an absent `Armed` proof, never a boolean.)

## Contents

- [Convergence walk](#convergence-walk)
- [Rule](#rule)

## Rule

Unarmed runs as close as possible to armed minus the destructive
operations: one code path, same gates, same verdicts, same
counters, would-tense narration — only the mutating call takes
the `Armed` proof. An early armed/unarmed fork that reimplements
evaluation is the smell; the proof-gated single mutation is the
shape.

## Convergence walk

Sweep, reap, backfill, and gc's own removals are converged (one
path, mutation gated on `Armed`) — what remains:

- [ ] Adopt/unlock paths — the two commands that mutate without
  any preview at all. `adopt` prunes old-epoch rows (destructive,
  preview-worthy); `unlock` mints (the mint is the command, and
  it also pairs/heals). Whether each needs a preview is undecided.

The stock collector's `--dry-run` stays outside the rule: a
foreign option we pass through, not our path. Our promises end
at the flag — the mark walk is the binary's, while husk/dir
enumeration (`findHusks`, `planPrune`) previews in the
would-tense through the same evaluation arming removes through.

Acceptance per operation: unarmed and armed summaries agree
field-for-field on the same state (modulo outcomes unknowable
without mutating), and the per-row preview log greps line-by-line
against the armed one in the would-tense.
