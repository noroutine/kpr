# Dry-run

Dry-run is the default mode, not a flag. Every mutating command
previews unless explicitly armed for that invocation. Future work
lives in [DRY_RUN_FUTURE.md](DRY_RUN_FUTURE.md).

## Principles

- **Safe by default.** No `--dry-run` flag exists anywhere: there
  is nothing to forget. The operator adds intent, never removes it.
- **Arming is per invocation.** `--no-dry-run` arms one run;
  `KPR_CLI_NO_DRY_RUN=true` arms every one-shot in the process
  (gc, reap, sweep, backfill alike). Anything else previews.
- **Dry-run is the absence of a produced proof, never a boolean of its own.**
  The adapter produces `proof.ArmedRun` from the flag or the env at
  its own boundary (`proof.Arm`); downstream reads the mode off
  the proof (nil means preview). No `DryRun bool`
  travels the call chain.
- **Preview runs the same path minus the destructive operation.**
  Same gates, same verdicts, same counters — the mutating call
  alone is gated. A preview that skips evaluation lies about what
  arming would do; a preview that evaluates differently doubly so.
- **Previews narrate in the would-tense.** `would sweep` /
  `would record` / `would skip`, plus how to arm. Armed runs
  narrate in the past tense. Counts match across modes:
  dry-run `N performed, N planned` previews armed `N/N`.
- **Detectable failures still surface.** Whatever fails without
  mutating (unreadable rows, failed gates) counts as failed in
  both modes. Outcomes unknowable without mutating (whether the
  registry delete succeeds) stay armed-only — the preview says
  what it would attempt, not what would succeed.

## Overrides

| Signal | Scope | Source |
|---|---|---|
| `--no-dry-run` | one invocation of gc, reap, sweep, `store backfill` | flag |
| `KPR_CLI_NO_DRY_RUN=true` | every one-shot in the process | env |

`sweep --output` writes the per-row log (would-tense in preview)
plus the summary into a file; stdout keeps the counters either way.

The sweep pass log carries the mode under the `dry_run` key
(`proof.Unarmed` of the pass proof). The key stays put even
though the code speaks armed now: dashboards read it, and
observability owns its vocabulary.
