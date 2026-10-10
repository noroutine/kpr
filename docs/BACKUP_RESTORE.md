# Backup/restore (deferred)

Question: how does a kpr store survive its host, move
between backends — and can the registry back itself up?
One mechanism, three uses: render any backend as a file
tree (the interchange format), then keep it as a tarball
or push it into the registry itself under `noroutine/kpr-`
as an OCI artifact, making the registry root a
self-contained backup target. Backup, restore, and
fs↔redis mobility are the same dump played three ways.
Verdict: not built; this doc keeps the shape so the
decision survives.

## Findings

- The store is already a plain dir: every backend concept
  has a file form (rows as JSON, locks as flock files, the
  intent marker as presence). A tarball is a faithful backup
  with zero new format — restore is untar and go.
- The filestore is the interchange format: dump any backend
  (redis HASH → row files, keys → lock files) to a file tree
  first, and backup/restore/mobility become one path. Row
  JSON is already byte-identical across backends, so the
  dump is a copy, never a conversion.
- Mobility rides the same path: fs → redis is replay the
  dump, redis → fs is the dump itself, and whatever backend
  comes next only needs both directions against files. No
  backend ever speaks another backend's protocol.
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
