# Backup/restore (deferred)

Question: how does a kpr store survive its host — and can the
registry back itself up? Two shapes, one mechanism: tar the
store dir (rows, locks, identity, current, activity ring),
then either keep the tarball or push it into the registry
itself under `noroutine/kpr-` as an OCI artifact, making the
registry root a self-contained backup target. Verdict: not
built; this doc keeps the shape so the decision survives.

## Findings

- The store is already a plain dir: every backend concept
  has a file form (rows as JSON, locks as flock files, the
  intent marker as presence). A tarball is a faithful backup
  with zero new format — restore is untar and go.
- The redis backend is the wrinkle: rows live in a HASH,
  locks as keys. Backup there means dump-to-files first
  (the wire already reads every key), restore means
  replay. File-first keeps one path.
- Pushing the tarball into `noroutine/kpr-` closes the loop:
  the registry carries kpr's memory of itself, next to the
  images. The existing self-contained story
  ([`docs/STORES.md`](STORES.md), `KPR_STORE_DIR` under the
  registry root) already co-locates them on disk; this lifts
  it into the registry API — snapshots survive volume loss,
  replicate with the registry, restore from any host that
  can pull.
- Naming reserves the namespace: `noroutine/kpr-` rows are
  kpr's own, never gc-reaped, never user-pushed. The sweeper
  must know the prefix the way it knows sentinels.

## Constraint

Backup earns its existence iff a deployment needs
host-independent recovery — move the store without moving
the disk. Until then the tarball is a runbook line and the
registry push is Gluttony: a second writer of blobs kpr
would then have to gc around. Revisit when the first
stateless-host deployment lands; the namespace reservation
is the part to get right up front.
