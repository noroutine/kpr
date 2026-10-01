# Proofs

Every claim kpr acts on is backed by one of five proofs. Each
establishes something different; they compose by weakening only.

| Proof | Establishes | Cost | Used by | Provided by |
|---|---|---|---|---|
| Mint (write a fresh generation, read it back) | liveness + currency + same-store, all at once | a generation (blobs + tag + row; keep-N reaps it) | `gc` armed, `unlock` | the producers themselves (`gc`, `unlock`) |
| Read gate (served generation + lineage verdict, no mint) | identity only — this mount is the paired store, not that it is current | two API reads | sweeper pass, backfill | the deleting caller, via the prover (`Sweeper`, backfill, `rm --untag`) |
| Presence (a generation answers, whichever) | existence, nothing more | one read | `gc` dry-run | `gc` reads it off the mount |
| Mode probe (cancelled upload initiate, 202 vs 405) | writable vs readonly | one aborted request | `gc` | `gc` probes the registry |
| Clock check (NTP/HTTPS/local vs tolerance) | skew is bounded, so timestamps mean something | one RTT | `gc`, `unlock` | the clock source (NTP/HTTPS/local) |
| Dry-run preview | nothing mutates; backs at most presence | free | everything, by default | nobody — there is nothing to provide |
| Armed run | the mutation was earned, under the matching proof above | whichever proof its row names | `gc`, sweep loop, `rm --untag`, `unlock` | the human, via flag or env at the boundary |

Mint ⊃ read gate ⊃ presence: each step down trades a guarantee
for cheapness. The lineage verdicts are the interpreter — they
turn whatever the proof returned into proceed/refuse/warn.
Nothing strengthens upward: a read gate can never prove liveness,
which is why backfill's staleness reasoning must not transfer to
`gc` (stale snapshot errs safe for backfill, fatal for deletes).

Two footnotes, kept honest: the mode probe proves nothing about
*our* store — it classifies the registry, full stop. And the
clock check is a precondition on trusting timestamps, not
evidence about storage. Five rows, two of which are "proof" only
by courtesy — named here so the blur stays visible.

Mechanics live where they are used: mint/read-gate/presence in
`docs/SENTINELS.md`, mode probe in `docs/GC.md`, clock in
`docs/TIMESTAMPS.md`, verdicts in `docs/SENTINELS.md`
(`kpr store adopt` ceremony included).

## Armed means proven

Dry-run is the default everywhere; arming is explicit per
surface (`--no-dry-run` / `KPR_CLI_NO_DRY_RUN` for one-shots,
`KPR_SWEEPER_NO_DRY_RUN` for the serve loop). The rule: an armed
run that mutates the registry or the mount carries proof — the
flag arms the mutation, the proof earns it.

| Armed by | Mutates | Proof it carries |
|---|---|---|
| `gc --no-dry-run` | mount + registry (collects) | mint, per run |
| serve loop armed | registry (deletes) | read gate, per pass |
| `rm --untag` (no dry-run by design) | registry + rows | read gate (`Proven`, Miss 2) |
| `unlock` (proving is the point) | mount + marker | mint |
| `reap --no-dry-run` | marks only (store-local) | none needed — no registry write |
| `rm`, `plan` edits (no dry-run by design) | rows/marks only | none needed — no registry write |

Store-local mutations need no proof because there is nothing to
prove *about*: no foreign store can answer for your own rows.
The moment a write leaves the store — manifest, blob, or mount
bytes — the corresponding row above applies, no exceptions.

## Evidence flows inward

Stages take their evidence as arguments, so a call that cannot
supply it cannot be constructed. The direction matters: the use
case dictates what evidence a stage needs, and cobra provides
it — never the reverse. When a stage needs `Armed` and no flag
wires it there, that is a cobra gap to fix, not a reason to
weaken the stage. Flags, env vars, and config are just evidence
sources at the adapter boundary; the signature decides what
must exist.

Example — `gc` sketched as evidence per stage:

| Stage | Evidence it takes | Provided from |
|---|---|---|
| preview (dry-run) | none | — |
| arm | `Armed` | `--no-dry-run` / `KPR_CLI_NO_DRY_RUN` |
| writable override | `Forced` (requires `Armed` — force without arming is meaningless) | `--force` |
| clock bound | `Checked` | `clock.Check` |
| intent | `Unlocked` | the store marker |
| lineage | `Proven` | the prover (Miss 2) |
| collect | `Proven` + `Fresh` (+ `Forced` iff writable) | mint produces `Fresh` |

Every refusal message stays identical — only the place that
guarantees it moves from runtime to signature.
