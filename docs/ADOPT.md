# `kpr store adopt`

> Not to be confused with [Adopting kpr](ADOPT_KPR.md) — that
> guide bolts kpr onto an existing registry; this command pairs a
> store to a lineage.

```bash
kpr store adopt                  # follow the served identity
kpr store adopt 01a1032e-...    # pin the expected one (mismatch refuses)
kpr store adopt --gen 01a10428-...  # accept a rollback as baseline
```

Pairing without minting: the only writer of the store identity,
for every pairing the verdict refuses to do on its own.

## Contents

- [The cases](#the-cases)
- [When not to use it](#when-not-to-use-it)
- [Aftermath](#aftermath)

## The cases

- **Unpaired store** follows the served identity (or a pinned
  `IDENT`, which must match it) and takes the served generation
  as baseline.
- **Store paired elsewhere** re-pairs and forgets the old epoch's
  sentinel rows (deletes them from the store, not from the
  registry — abandoned generations stay served until gc collects
  them).
- **Already paired to the served identity** refreshes the
  baseline to the served generation, keeping the recorded
  adoption time.
- **Silent registry with a pinned `IDENT`** pre-pairs with no
  baseline: the first mint establishes it.
- **`--gen` asserts the served generation**: it must name what
  the registry serves, not what you wish it served — a mismatch
  refuses (the registry moved under you). A served older
  generation is accepted as baseline by the pairing itself,
  which is how a rollback is taken on.

## When not to use it

- **Fresh silent registry, no pinned identity** — nothing is
  served yet, so there is nothing to pair to. `adopt`
  correctly refuses; `store unlock` mints the baseline and
  pairs in one move.
- **Identity-less payloads** refuse even here: wipe the volume
  or remove the stale tags instead. No pairing is better than a
  pairing to nobody.

## Aftermath

Adopt pairs; it stamps no rows and never unlocks —
`kpr store unlock` always follows to open writes. Only re-pairing
forgets rows, and only the old epoch's sentinel ones (other repos
are untouched; the registry keeps serving them until gc). The served
generation stays untracked until the next armed `gc`
adopt-records it (`backfill` would record it as an absent tag
too), so the trust word reads `behind` in between — lag on a
fresh pairing, not failure (on a wiped store the same word is
the Heal signal: fresh-empty and wiped-empty look alike, and
only the operator knows which it is).
