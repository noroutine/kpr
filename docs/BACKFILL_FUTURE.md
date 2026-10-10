# Backfill — future work

Unbuilt backfill extensions. Shipped behavior lives in
[BACKFILL.md](BACKFILL.md).

## Contents

- [Filling missing digests on tracked rows](#filling-missing-digests-on-tracked-rows)

## Filling missing digests on tracked rows

Backfill adds rows for catalog tags the store does not track,
and skips everything already tracked. It resolves each added
row's digest on the way in and refuses to record a tag whose
digest it cannot read. So backfill never creates a digest-less
row — but it never fixes one either.

Tracked rows without a digest still occur:

- The receiver records a push before the manifest PUT finishes;
  if the completion never updates the row, the digest stays
  empty.
- Anything changing registry data past kpr: a stock gc run
  directly, tag deletes, restores. Rows then point at moved or
  missing data.

Two cases, different handling:

1. The tag never existed or is already gone (interrupted
   push). The `partial` policy marks the row at 24h and the
   sweep resolves it gone on the registry's 404. Self-cleaning;
   nothing to build.
2. The tag exists but the row has no digest. Selectors still
   mark it due (by tag, or `partial` at 24h), but the sweep
   deletes by digest and only falls back to the tag — stock
   `registry:3` rejects tag deletes, so the row fails on every
   pass and stays failed. Worse, without a digest the "digest
   moved" skip-check has nothing to compare against, so a
   repushed tag silently changes what the row refers to.

The work is a contract extension on backfill, from add-only to
fill: visit tracked rows with empty digests (not just absent
tags) and resolve each through the same `ManifestDigest` HEAD
backfill already makes, updating the row. Done when a tracked
digest-less row with a live tag gets its digest on the next
backfill, and the sweep for it goes by digest and succeeds.
