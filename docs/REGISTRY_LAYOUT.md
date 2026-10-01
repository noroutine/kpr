# Registry filesystem layout

Filesystem driver, `registry:3` lineage. Paths below read live off
the dev stack (`docker exec kpr-registry …`), not quoted from source.

## Root

`<root>/docker/registry/v2/` holds two things:

- `repositories/` — one dir per repo. The repo name IS the path
  (`noroutine/kpr-sentinel` →
  `repositories/noroutine/kpr-sentinel/`); the tag IS a subdir.
  No mapping table anywhere: `_catalog` is a readdir of
  `repositories/`, `tags/list` a readdir of one tags dir, and
  `GET /v2/<name>/manifests/<tag>` walks straight into
  `repositories/<name>/_manifests/tags/<tag>/current/link`.
- `blobs/sha256/<first-byte>/<full-digest>/data` — the only
  global. Content-addressed; a blob dir holds exactly `data`,
  no back-pointers: a blob doesn't know its manifests, a
  manifest doesn't know its tags.

## The chain (observed, `noroutine/kpr-sentinel:latest`)

Tag pointer → doc → bytes, every arrow forward:

- `_manifests/tags/latest/current/link` → `sha256:57df…`
- `_manifests/tags/latest/index/sha256/57df…/link` → same (tag history)
- `_manifests/revisions/sha256/57df…/link` → same (doc existence)
- `blobs/…/data` at that digest → manifest JSON:
  `{config: {digest: sha256:93f6…}, layers: [], annotations: {…}}`
- `_layers/sha256/93f6…/link` → `sha256:93f6…` (repo membership —
  blob GETs 404 without it)
- `blobs/…/data` at that digest → the bytes.

## Who keeps what alive

Default collect marks from **all** revision docs, tags
unconsulted; `--delete-untagged` additionally deletes manifests
no tag reaches. Dev snapshot at time of writing: 12 tag links
vs 169 revision docs — tags see a fraction of the documents.
Roots are wider than tags: all revisions, index→child edges,
digest holders, referrers readers. One server-side coupling:
deleting a manifest by digest unlinks every tag referencing it
(observed e2e) — a doc delete never leaves its own tags dangling;
dangling links come only from crashed deletes, never from the API.

## API gap that follows

No manifest-list endpoint exists: untagged docs are enumerable
on fs (readdir of `revisions/`) but invisible over HTTP. So
enumerating own superseded docs needs memory (a ledger) or the
same store (an fs walk) — pure-API remotes can neither list
nor reap them.
