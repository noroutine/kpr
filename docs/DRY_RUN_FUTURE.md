# Dry-run — future work

Converging every preview/armed pair onto one code path. Shipped
principles live in [DRY_RUN.md](DRY_RUN.md).

## Contents

- [Convergence walk](#convergence-walk)
- [Rule](#rule)

## Rule

Dry-run is as close as possible to no-dry-run minus the
destructive operations: one code path, same gates, same verdicts,
same counters, would-tense narration — only the mutating call is
gated. An early `if dryRun { ... } else { ... }` fork that
reimplements evaluation is the smell; the gate around the single
mutation is the shape.

## Convergence walk

Walk every dry-run/no-dry-run operation and bring it to the rule.
Status as of the sweep convergence (`4a69bab`, `63bde35`):

- [x] `sweep` — same gate both modes; dry-run counts planned and
  performed; `RowLog` streams would/swept plus skip reasons;
  read-path failures count as failed either way.
- [ ] `gc` — collector delegates preview to the stock binary's
  `--dry-run` (a separate implementation of the mark walk, not
  the same path); husk/dir removal skipped wholesale in preview.
  Verify the preview promises what arming performs.
- [ ] `reap` — verify the plan-only path marks nothing while
  evaluating exactly what arming would mark.
- [ ] `store backfill` — verify the preview loop records nothing
  while `would record`/`would skip` cover exactly what arming
  would stamp.
- [ ] Adopt/quickstart paths — check which mutate without any
  preview at all, and whether each needs one.

Acceptance per operation: preview and armed summaries agree
field-for-field on the same state (modulo outcomes unknowable
without mutating), and the per-row preview log greps line-by-line
against the armed one in the would-tense.
