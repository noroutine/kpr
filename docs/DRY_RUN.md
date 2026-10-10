# Dry-run

Dry-run is the default mode, not a flag. Every mutating command
previews unless explicitly armed for that invocation.

## Rule

Unarmed runs as close as possible to armed minus the destructive
operations: one code path, same gates, same verdicts, same
counters, would-tense narration — only the mutating call takes
the `Armed` proof. An early armed/unarmed fork that reimplements
evaluation is the smell; the proof-gated single mutation is the
shape.

Two deliberate exceptions. `store unlock` never previews (the
mint is the command; there is no evaluation to rehearse). And
the stock collector's `--dry-run` is a foreign option we pass
through, not our path: `kpr gc` previews its own removals
through the rule above, while the mark walk is the binary's —
our promises end at the flag.

## Terminology

"Dry-run" is the user-facing word: flags, help text, and
summaries say dry-run because that is what the operator typed
(or didn't). Inside the code the language is armed/unarmed — a
run carries a `proof.ArmedRun` or it doesn't (nil means
preview); no `DryRun bool` travels anywhere. One exception keeps
the old word: the stock registry binary's `--dry-run` flag, a
foreign option we pass through.

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
  An early armed/unarmed fork that reimplements evaluation is the
  smell; the proof-gated single mutation is the shape.
- **Previews narrate in the would-tense.** `would sweep` /
  `would record` / `would skip`, plus how to arm. Armed runs
  narrate in the past tense. Counts match across modes:
  dry-run `N performed, N planned` previews armed `N/N`.
- **Detectable failures still surface.** Whatever fails without
  mutating (unreadable rows, failed gates) counts as failed in
  both modes. Outcomes unknowable without mutating (whether the
  registry delete succeeds) stay armed-only — the preview says
  what it would attempt, not what would succeed.

Acceptance per operation: unarmed and armed summaries agree
field-for-field on the same state (modulo outcomes unknowable
without mutating), and the per-row preview log greps line-by-line
against the armed one in the would-tense.

## Overrides

| Signal | Scope | Source |
|---|---|---|
| `--no-dry-run` | one invocation of gc, reap, sweep, `store backfill`, `store adopt` | flag |
| `KPR_CLI_NO_DRY_RUN=true` | every one-shot in the process | env |

`sweep --output` writes the per-row log (would-tense in preview)
plus the summary into a file; stdout keeps the counters either way.
