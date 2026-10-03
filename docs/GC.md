# Garbage collection

## Contents

- [Garbage collection today](#garbage-collection-today)
  - [The proof chain](#the-proof-chain)
  - [Two paths, chosen by the probe](#two-paths-chosen-by-the-probe)
  - [Same-store proof](#same-store-proof)
  - [Dry-run default](#dry-run-default)
  - [Mechanics](#mechanics)
- [After an armed run: stale blob descriptors](#after-an-armed-run-stale-blob-descriptors)
- [Per-repo collection: keep-N over gen tags](#per-repo-collection-keep-n-over-gen-tags)

Unbuilt gc designs — dangling tags, token-auth registries — live in
[GC_FUTURE.md](GC_FUTURE.md).

## Garbage collection today

Manifest deletes drop references only; blob bytes need the stock
collector against the shared store. `kpr gc` shells the stock
`registry garbage-collect`, COPYd from the same `registry:3` the
stack runs.

Full spec lives in
[ARCHITECTURE.md](ARCHITECTURE.md#garbage-collection).

### The proof chain

Every run proves these in order, and refuses at the first failure:

1. Binary and config mounts exist.
2. The store root is a local filesystem.
3. The sentinel sees a classifiable mode — a cancelled blob-upload
   initiate under a probe repo: 202 writable, 405 maintenance
   readonly, anything else refuses.
4. The mode's own checks clear — the blobdescriptor cache for
   readonly, the online preflight for writable (both below).
5. The local mount is the registry's own store.

### Two paths, chosen by the probe

The preflight derives the path from the serving probe. There is no
`--online` flag to forget: the mode decides, the checklist explains.

**Stopped or readonly — the classic collect.** The blobdescriptor
cache must answer. Set `REGISTRY_REDIS_PASSWORD`; without it the
mark phase deletes live layers.

**Serving — the fenced collect.** The preflight clears two things
up front:

| Check | Clears when | Override |
| --- | --- | --- |
| Blob cache | no blobdescriptor cache configured | `--accept-blob-cache` |
| Gateway | proven edge listening, HOLD lease configured | `--accept-unfenced` |

An armed run reports every miss at once and refuses unaccepted
ones; a dry-run prints the checklist and previews on. The collect
engages the HOLD lease around itself — a fence that fails to engage
refuses rather than collect unfenced.

Run-wide risks carry their own flag on every path:
`--accept-clock-skew`, `--accept-rollback`, `--accept-mode-flip`.
Each names the risk it accepts, armed runs only, no umbrella.

### Same-store proof

Every armed run writes a fresh `noroutine/kpr-sentinel:latest`
generation to the local mount — linked at its uuid tag beside the
floater — and reads it back through the API. Both modes; an empty
redis proves fine.

- **Lineage first.** The mint carries the store's lineage id, and
  the served generation is judged against the paired identity
  before anything else. Foreign refuses even fully accepted; stale
  refuses armed unless `--accept-rollback`; silence establishes.
  Verdict table in [SENTINELS.md](SENTINELS.md). Refused pairings
  heal through `kpr store adopt`, never an accept flag.
- **Clock check opens every run.** Skew past 30s refuses unless
  `--accept-clock-skew`; an unreachable source warns and proceeds.
  `KPR_TIME_METHOD` is local/https/ntp (default local),
  `KPR_TIME_SERVER` defaults to `zeitstempel.dfn.de`, compose pins
  https. See [TIMESTAMPS.md](TIMESTAMPS.md).
- **One row per mint.** The verified mint records gen tag, digest,
  and writer. The floater is never tracked.
- **Order on a writable registry:** online preflight, then mint.
- **Overflow dies by keep-N** (below), not by `--delete-untagged`.

### Dry-run default

`kpr gc` previews; `--no-dry-run` collects for real.

Previews never mint — no blobs, no tags, no rows — but they do
read. The served generation proves presence: nothing served refuses
with "no sentinel served" and names the armed ceremony, unreadable
stays an error. Freshness stays armed-only, because only a fresh
mint distinguishes a stale snapshot from the shared store. Only
armed runs pay for the proof, and only they print it.

### Mechanics

- **Advisory lock.** The shared `kpr:gc:lock` (30m bound)
  serializes kpr-driven runs. A manual `garbage-collect` takes no
  such lock, so never run one alongside.
- **Evented runner.** The collector streams through a subprocess
  with pipe capture, line streaming, and drain discipline, plus
  pre/post sentinel events. A mode flip mid-run fails loudly unless
  `--accept-mode-flip`.
- **Skeleton pruning.** The stock collector deletes blobs and links
  but leaves their parent dirs, so every run grows an empty tree.
  An armed run prunes it after the collect (`pruned N empty
  directories`, `prune` stage event): bottom-up, empty dirs only,
  root/files/symlinks never touched, occupancy races resolve safe
  because remove *is* the check. Previews prune nothing. A prune
  failure warns, never fails the collection.
- **Upload sessions are not gc's.** Abandoned `_uploads/<uuid>`
  dirs hold partial bytes, never empty dirs, so pruning skips them
  — and gc never deletes them either. A live push PATCHes there,
  and the HOLD fence doesn't cover direct-to-registry doors, so a
  post-collect wipe could kill in-flight uploads or race the commit
  window. Orphaned sessions belong to the registry's
  `uploadpurging` (24h age, hourly — see the registry configs),
  which only reaps idle ones.

## After an armed run: stale blob descriptors

The collector deletes blob files but never invalidates the
blobdescriptor cache — registry:3 exposes no flush API for it
(HTTP surface is the v2 API plus debug/health only). Until the
cache drops, the registry vouches for deleted blobs: HEAD answers
200, so push clients skip the upload (`existing blob`), manifest
PUTs 201 against surviving revision links, the tag PUT 201s — and
the tag is broken (`MANIFEST_UNKNOWN`, content absent from
`_blobs`). Observed end to end: blob HEAD 200 with no link and no
data file on disk, flipping to 404 after a registry restart, at
which point a re-push uploaded everything for real (0 skipped)
and the tag resolved.

Remedy — restart alone is not always enough:

- **File stack** (`registry-config.file.yml`): no descriptor
  cache at all (deleted section — proven: blob HEAD 404s the
  moment gc finishes, re-push uploads for real). Nothing to
  restart, nothing to flush.
- **Cached deployments** (inmemory or redis): a gc against a
  *stopped* registry gets a fresh inmemory cache free; a gc
  against a *readonly-but-running* registry needs an explicit
  restart, otherwise the hot cache keeps vouching for deleted
  blobs. Restart does *not* drop redis keys — flush the
  descriptor DB too (`redis-cli -n 3 FLUSHDB`; DB 4 holds kpr
  rows and is untouched). Descriptors are pure cache,
  repopulated on demand.

Dry-run previews delete nothing, so the cache stays valid — no
remedy needed there.

kpr does not restart the registry itself — the collector's proof
ends at the store boundary. A re-push between gc and restart
mints a dead tag; a restart plus a fresh push self-heals.

## Per-repo collection: keep-N over gen tags

The stock collector marks globally; it cannot scope to one repo —
so the sentinel repo scopes itself. Each generation carries its own
uuid tag with a tracked row; `reap keep-n` marks all but the ten
freshest (the `latest` floater is spared by name, no excludes
needed); the sweep deletes overflow docs by digest and drops the
rows; a global *default* collect reaps the dangling blobs. Blast
radius confined to a repo kpr owns; user repos and the
`--delete-untagged` flows never involved. E2e-pinned
(`test/e2e/sentinel_keepn_test.go`): the digest delete unlinks
referencing tags server-side, so reaped gens leave neither doc nor
tag link — no dangling-tag residue on this path (crashed deletes
are the [dead-link pass](GC_FUTURE.md#dangling-tags)'s job).
Per-run cost stays +2 blobs,
+1 tag, +1 row; steady state is ten tagged generations. Full design
in `docs/SENTINELS.md`.
