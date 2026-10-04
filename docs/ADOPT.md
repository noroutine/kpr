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

- [The three cases](#the-three-cases)
- [When not to use it](#when-not-to-use-it)
- [Aftermath](#aftermath)

## The three cases

- **Unpaired store** follows the served identity (or a pinned
  `IDENT`, which must match it) and takes the served generation
  as baseline.
- **Store paired elsewhere** re-pairs and prunes the old epoch's
  sentinel rows — abandoned generations leave with it.
- **`--gen` names the served generation**, accepting a rollback
  as baseline. It must name what the registry serves, not what
  you wish it served.

## When not to use it

- **Fresh silent registry** — nothing is served yet, so there is
  nothing to pair to. `adopt` correctly refuses; `store unlock`
  mints the baseline and pairs in one move.
- **Identity-less payloads** refuse even here: wipe the volume
  or remove the stale tags instead. No pairing is better than a
  pairing to nobody.

## Aftermath

Adopt pairs and prunes; it stamps no rows. The served generation
stays untracked until the next armed `gc` adopt-records it
(`backfill` would record it as an absent tag too), so the trust
word reads `behind` in between — lag, not failure. The
adopt-before-unlock window this leaves on empty stores is open
work: [ADOPT_FUTURE.md](ADOPT_FUTURE.md).
