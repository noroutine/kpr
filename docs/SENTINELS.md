## Contents

- [Current situation](#current-situation)
- [What distribution actually serves](#what-distribution-actually-serves)
- [Same-store proof without API writes](#same-store-proof-without-api-writes)
- [Why not a crafted image](#why-not-a-crafted-image)

## Current situation

The gc sentinel does two jobs under one name, and only one of
them needs a write:

- **Write proof (mode).** `POST /v2/kpr-gc-probe/blobs/uploads/`
  (`internal/gc/sentinel.go`): 202 means the registry takes
  writes, 405 means v3 maintenance readonly. The upload is
  cancelled at once, leaving nothing. This is classification,
  not identity — it says what the registry *is*, not *whose*
  store kpr sees.
- **Same-store proof (identity).** Writable: the uuid dir the
  probe just created must exist under the configured root
  (`SameStoreUpload`, `internal/gc/proof.go`). Readonly:
  a tracked tag's link file must resolve to the tracked
  digest (`SameStoreTagLink`). Both directions lean on the
  write probe or on previously tracked rows — readonly with
  an empty redis proves nothing today.

The plan: separate them. The write probe keeps classifying
mode. Identity gets its own proof in the opposite direction —
fs write, API read — needing no API writes at all, in either
mode, with no tracked rows required.

## What distribution actually serves

Studied against distribution/distribution HEAD (registry:3
lineage), filesystem driver. No reverse proxy, no new
endpoints — only what the registry already exposes:

- **No static serving outside `/v2/`.** The app router
  registers v2 paths only (`registry/handlers/app.go`,
  `registry/api/v2/routes.go`); anything else 404s. A blob
  dropped on disk is *not* reachable as a file — only through
  the blob API, which demands a per-repo `_layers` link first
  (`linkedBlobStatter.Stat`: missing link → `ErrBlobUnknown`,
  no fallback to the global store).
- **Beyond `/v2/`:** `GET /` (alive, empty 200), `GET /v2/`
  (empty JSON 200). Debug addr (`:5001` in our config):
  `/metrics`, `/debug/health`. All liveness/counters — none
  touch stored content, none prove identity.
- **The exploitable read paths** (fs object → API signal):
  - `_catalog`: walks `docker/registry/v2/repositories/`
    and lists a repo iff the walk hits its `_manifests` dir
    (`registry/storage/catalog.go:handleRepository`, walk
    yields directories too). **No cache on this path.**
  - `tags/list`: `List`s `<repo>/_manifests/tags/` —
    missing dir → 404 `NAME_UNKNOWN`; existing empty dir →
    200 `{"tags":[]}` (`registry/storage/tagstore.go:All`).
  - manifest/blob GETs: need revision links, layer links,
    global blob `data` files — craftable but heavy, and
    every GET populates the redis blobdescriptor cache
    (`HMSet`, no `EXPIRE` — entries never time out).

So the cheapest fs→API signal is the catalog: a directory
kpr creates appears in a registry listing. Tags/list with a
pre-created empty `tags/` dir confirms it with a 200 instead
of a 404.

## Same-store proof without API writes

1. `mkdir -p <root>/docker/registry/v2/repositories/<sentinel>/_manifests/tags`
   (fs write — kpr gc already implies write access).
2. `GET /v2/_catalog` (paged, existing client) contains
   `<sentinel>`; optionally `GET
   /v2/<sentinel>/tags/list` returns 200 with no tags.
3. `rm -rf` the sentinel repo dir at once.

Why this shape:

- **Both modes.** Reads work under maintenance readonly —
  one proof for writable and readonly, no tracked rows, no
  empty-redis refusal.
- **Self-cleaning.** Removed right after the read, the
  collector never sees it. Even if cleanup races a collect:
  no revisions, no blobs — the mark phase enumerates zero
  manifests, marks nothing (`MarkAndSweep` walks revisions
  per repo); `--delete-untagged` has no manifest to delete.
- **Invisible to kpr itself.** Fs-crafted content emits no
  registry notification events → the receiver never tracks
  rows → backfill sees a repo with zero tags, sweeper has
  nothing to mark. The proof leaves no work for the pipeline.
- **No cache residue.** Catalog and tags read straight from
  the walk; redis untouched.
- **No API writes.** The write probe stays a classifier;
  identity never mints uploads, links, or blobs.

Name: `<sentinel>` = `kpr-sentinel`, beside the existing
`kpr-gc-probe` write-probe repo. Fixed name is fine —
mkdir is idempotent, concurrent runs prove the same truth.

## Why not a crafted image

The considered alternative: write a real manifest blob +
revision link + tag link on fs, read it back via
`GET /v2/<repo>/manifests/<tag>`. Stronger signal (full
content round-trip), worse citizenship:

- **gc:** a tagged manifest is live by definition — the mark
  phase keeps it and every referenced blob forever. The
  sentinel becomes permanent store pollution, or (untagged)
  gets eaten by the very collector it guards.
- **cache:** each blob GET writes redis blobdescriptor
  entries with no TTL — permanent residue per digest.
- **kpr pipeline:** a tagged repo appears in backfill
  enumeration → tracked rows → sweeper marks due → DELETEs
  the proof out from under itself, loudly.

Content round-trip proves nothing the catalog signal
doesn't: both read through the same driver off the same
root. The bare dir is the whole proof; an image is proof
plus three kinds of residue.
