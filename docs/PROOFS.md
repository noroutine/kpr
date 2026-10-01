# Proofs

Every claim kpr acts on is backed by one of five proofs. Each
establishes something different; they compose by weakening only.

| Proof | Establishes | Cost | Used by | Provided by | Sealed as |
|---|---|---|---|---|---|
| Mint (write a fresh generation, read it back) | liveness + currency + same-store, all at once | a generation (blobs + tag + row; keep-N reaps it) | `gc` armed, `unlock` | the producers themselves (`gc`, `unlock`) | `FreshGeneration` (`fresh.go`) |
| Read gate (served generation + lineage verdict, no mint) | identity only — this mount is the paired store, not that it is current | two API reads | sweeper pass, backfill | the deleting caller, via the prover (`Sweeper`, backfill, `rm --untag`) | `SameStore` (`same_store.go`) |
| Presence (a generation answers, whichever) | existence, nothing more | one read | `gc` dry-run | `gc` reads it off the mount | — |
| Mode probe (cancelled upload initiate, 202 vs 405) | writable vs readonly | one aborted request | `gc` | `gc` probes the registry | — |
| Clock check (NTP/HTTPS/local vs tolerance) | skew is bounded, so timestamps mean something | one RTT | `gc`, `unlock` | the clock source (NTP/HTTPS/local) | `BoundedClock` (`checked.go`) |
| Intent (unlock marker present) | the operator opened this store, after proving it | one read | `gc`, writers | the unlock ceremony, via the marker | `UnlockedStore` (`unlocked.go`) |
| Armed run | the mutation was earned, under the matching proof above | whichever proof its row names | `gc`, sweep loop, `rm --untag`, `unlock` | the human, via flag or env at the boundary | `ArmedRun` (`armed.go`) |
| Accepted risk | leave to proceed despite a loud refusal (writable registry, skewed clock) | one flag, on an armed run | `gc` | the human, via `--force` at the boundary | `AcceptedRisk` (`risk.go`) |

Mint ⊃ read gate ⊃ presence: each step down trades a guarantee
for cheapness. The lineage verdicts are the interpreter — they
turn whatever the proof returned into proceed/refuse/warn.
Nothing strengthens upward: a read gate can never prove liveness,
which is why backfill's staleness reasoning must not transfer to
`gc` (stale snapshot errs safe for backfill, fatal for deletes).

Two footnotes, kept honest: the mode probe proves nothing about
*our* store — it classifies the registry, full stop. And the
clock check is a precondition on trusting timestamps, not
evidence about storage. Two rows are "proof" only by courtesy —
named here so the blur stays visible.

Dry-run is not a row: it is the absence of `ArmedRun` (`nil`),
not a kind of evidence. Previews prove nothing, so preview
paths take no evidence — the stage table's preview row takes
none, and that is the whole statement.

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
| `rm --untag` (no dry-run by design) | registry + rows | read gate (`SameStore`, Miss 2) |
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
it — never the reverse. When a stage needs `ArmedRun` and no flag
wires it there, that is a cobra gap to fix, not a reason to
weaken the stage. Flags, env vars, and config are just evidence
sources at the adapter boundary; the signature decides what
must exist.

Example — `gc` sketched as evidence per stage:

| Stage | Evidence it takes | Provided from |
|---|---|---|
| preview (dry-run) | none | — |
| arm | `ArmedRun` | `--no-dry-run` / `KPR_CLI_NO_DRY_RUN` |
| accepted risk | `AcceptedRisk` (requires `ArmedRun` — force without arming is meaningless) | `--force` |
| clock bound | `BoundedClock` | `clock.Check` |
| intent | `UnlockedStore` | the store marker |
| lineage | `SameStore` | the prover (Miss 2) |
| collect | `SameStore` + `FreshGeneration` (+ `AcceptedRisk` iff overriding) | mint produces `FreshGeneration` |

Every refusal message stays identical — only the place that
guarantees it moves from runtime to signature.

## How a proof is built

Cut proofs live in `internal/proof`, one file per kind. The
pattern is the same every time, because the threat is the same
— a stage that forgets its check, today or in a later refactor:

1. Sealed interface. An unexported method (`sealed()`) means no
   outside package can implement or construct it; the zero
   value is `nil`, and a test pins that down.
2. One constructor per source, and unexported. The source
   constructors name the granting boundary (flag, env) but no
   outside package can call them — proofs are produced by
   provers, never crafted at a call site. The provers live
   beside the proof they produce (`Arm` in `armed.go`, `Force`
   in `risk.go`): adapters feed raw readings — cobra flag vars,
   config env — and the package owns precedence and minting.
   No source, no inhabitant.
3. Composition in signatures. `Force` takes `ArmedRun`:
   leave without intent does not compile. Each prover's
   parameters are the prerequisites; each stage's parameters
   are the evidence. Nothing to remember, nothing to re-check.
4. Provenance is label-only. `Source()` names the granting
   boundary for loud output — never for branching.

All six cut: `SameStore` (system evidence, from the prover),
`ArmedRun` (human intent, from the boundary), `AcceptedRisk`
(accepted risk, from intent), `BoundedClock` (clock bound,
refusals passing through), `UnlockedStore` (marker intent, read
once), `FreshGeneration` (mint receipt, named at the write).

Three constructor shapes, by how the evidence is earned.
Re-checking: the `Prover` and `ProveUnlockedStore` read evidence
themselves — the constructor is the check. Passing through:
the `Checker` runs `clock.Check` and returns its refusals
untouched — skew still refuses, dead sources still warn, and
the callers that interpret them change nothing. Mint-site:
`MintedGeneration` names the generation beside the Write and Verify
it just performed — the seal holds the shape, but only the
single call site holds the truth; review it, there is one per
minter. Presence and the mode probe stay uncut on purpose:
previews take no evidence, and the probe classifies the
registry, never our store.
