# Status

What kpr currently ships, and what is still open. Kept separate
from the reference pages because this is the part that goes stale.

The design itself lives in [ARCHITECTURE.md](ARCHITECTURE.md).
Designs for work that does not exist yet live in the `_FUTURE`
pages — see the [index](README.md#status-and-unbuilt-work).

## Contents

- [Current state](#current-state)
- [Open, in no order](#open-in-no-order)

## Current state

Built on `master`: CI green, full unit suite and lint clean, e2e
green in compose.

Proven live, end to end:

```
push → receiver tracks → reap --no-dry-run marks → sweep deletes by
digest → kpr gc previews, --no-dry-run collects
```

Marks can come from a single policy (`reap <name>`), be hand-picked
(`plan add`), or be pruned (`plan remove`).

What is settled:

- **Policies.** All five live and selectable. `latest` is spared
  everywhere (10 + `latest`). Ensure-survivor tripwires per tag
  style, plus the `isBareHash` a/f boundary pins.
- **keep-N.** Live, N fixed at 10, excludes via `reap --exclude`.
  Per-repo tuning declined by decision.
- **Proof chain.** Sentinel generations tagged with keep-N reaping;
  lineage-gated altering paths (`gc`, `unlock`, sweeper) with the
  explicit `kpr store adopt` pairing ceremony; a checked clock
  (local default, compose pins `https`) opens every altering path.
- **GC lock** verified advisory against the distribution source —
  `MarkAndSweep` at v3.1.2 sets none.
- **Mutation testing** (gremlins, local): 94.84% efficacy on the
  clock tree, 588 killed and 32 lived. Survivors are timing mutants,
  provable equivalents (NOTE'd at the site), dead-server error
  convergence, and live-redis branches that only die under
  `-tags e2e`. Scaffolding is excluded from candidacy — see
  [TESTING.md](TESTING.md). Killable survivors were fixed with
  focused tests.

## Open, in no order

| Item | Status |
| --- | --- |
| keep-N tuning surface (`--last`, `--include`) | declined — N stays 10, with `--exclude` |
| Real partial-upload detection (bounded manifest reads) | open |
| Sweep live-stages transport (polling vs websocket) | deferred — the vocabulary and keys are the contract |
| Registry metrics as a GC-readiness signal (storage pressure before collecting) | noted, not scheduled |
| Tag-release flow (image push + Forgejo release) | unverified |
