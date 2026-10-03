# Garbage collection — dangling references

Designs for dangling-reference work that is **not built**.
Shipped gc behavior lives in [GC.md](GC.md); other unbuilt gc
designs live in [GC_FUTURE.md](GC_FUTURE.md).

## Contents

- [What can dangle](#what-can-dangle)
- [Dead tag links (designed)](#dead-tag-links-designed)
- [Risks and open questions](#risks-and-open-questions)

## What can dangle

A dangling reference is a pointer whose target is gone. They
fail at pull time, not delete time — the write path succeeds,
the read path 404s. The registry ships no find-dangling
command; each class needs its own walk.

Verified live against registry:3 on the shadow stack (oras /
regctl / crane probes plus fs inspection, Oct 2026). Docs that
say otherwise are stale where they disagree — see the tag
rows below.

| Dangling pointer | Pull symptom | Detection | Cure (removal — no class is repairable) | Cost |
|---|---|---|---|---|
| tag link → revision | tag listed, GET 404 `MANIFEST_UNKNOWN` | API: tags/list + manifest HEAD per tag (fs walk only when stopped) | API: `DELETE` the tag → 202, dead link gone from disk (sweeper); fs: remove the link (gc pass, stopped) | R lists + T HEADs to find; 1 DELETE per cure |
| manifest → blob | pull fails `BLOB_UNKNOWN` (verbatim) | API: GET each manifest, HEAD each blob | API: `DELETE` the parent by digest → 202 (sweeper); orphan bytes go gc-eligible. Operator re-push heals instead | T manifest GETs + B blob HEADs — the heavy one |
| index → child | pull fails `MANIFEST_UNKNOWN` at child fetch, repo-local | API: GET each index, GET each child | API: `DELETE` the parent by digest → 202 (sweeper); orphan bytes go gc-eligible. Operator re-push heals instead | I index GETs + C child GETs, index repos only — bounded |
| referrer → subject | subject pull fails; referrer itself reads fine | API: check the subject per referrer tag | nothing to remove — additive metadata (optionally `DELETE` the referrer tag) | 1 HEAD per referrer tag — tiny |
| layer link → blob data | pull fails `BLOB_UNKNOWN`, link present | **fs walk only**: `_layers/*/link` vs `blobs/…/data` (nothing enumerates `_layers`) | API: `DELETE` the parent by digest → 202 (sweeper); the orphaned link is gc-eligible (observed: `layer link eligible for deletion`) | one fs walk, zero HTTP; 1 DELETE per cure |

Nothing here is repairable — missing bytes come back only via
re-push, which is the operator's move, not kpr's. Every kpr-side
cure is cleanup: remove the broken parent over the API (sweeper,
serving) or the pointer itself off disk (gc pass, stopped).
Stock gc cures nothing: it ghost-marks missing edges without
error and walks past dead tag links entirely.

Reading notes from the probes:

- Manifest references resolve repo-locally: an index child is
  fetched from the *index's* repo, and index creation mounts the
  child manifest + blobs there — a deleted original leaves the
  mounted copy serving until it too is deleted.
- Manifest docs are per-repo: deleting a digest unlinks that
  repo's tags and removes that repo's revision link; other repos'
  copies are unaffected. Deleted docs' JSON bytes become
  gc-eligible orphans (observed: `blob eligible for deletion`).
- Untagging (`DELETE` by tag) removes the revision doc as well —
  no orphan left for gc.
- `DELETE` of an untagged but index-referenced doc succeeds (202,
  no protection) — the API lets you break an index.
- No `/referrers/` route on this build (plain-404 fallback):
  referrers travel as `sha256-<hex>` tags only.
- Manifest reads need a manifest Accept (`*/*` works; no Accept
  404s even on existing docs).

Adjacent, not dangling: the blobdescriptor cache vouching for
deleted blobs (HEAD 200, no data on disk) — the reference is
intact, the cache lies. Handled by restart or a cache-less
config, never by a dangling pass.

The reverse is orphaned, not dangling, and gc already owns it:
untagged manifests (`--delete-untagged`), unreferenced blobs
(mark-and-sweep), stale uploads (the registry's `purgeuploads`
setting).

## Dead tag links (designed)

A dangling tag is a tag link whose manifest revision is gone —
a crashed delete, or backend inconsistency:

```
<root>/…/repositories/<repo>/_manifests/tags/<tag>/current/link
```

The tag keeps showing up in listings, lying, and pulls fail.
Backfill skips unresolvable tags with a count; the design below
owns them.

### Why no existing tool owns them

- **The sweeper resolves but never finds.** `DELETE` by tag untags
  on registry:3 (202 observed live — the 405 belief was
  stub-encoded, never probed). But the sweeper only knows tracked
  rows; never-tracked dead tags are invisible until something
  enumerates the registry.
- **The stock collector can't.** Mark-and-sweep walks manifest
  revisions, never tag links — verified on a live dead link: the
  repo walks, nothing marks, the link survives untouched.
- **`reap` can't.** It only knows what the store knows, and
  never-tracked dangling tags are invisible to every policy until
  something enumerates the registry.

So the missing piece is enumeration, not resolution: the scan
finds them, the sweeper (online) or the gc dead-link pass
(stopped registry) removes them.

### Design: two detectors, one mark kind, gc resolves

1. **Scanner (full-repo pass).** Enumerate via `_catalog` paging and
   per-repo tag lists, reusing the backfill registry client
   (`CatalogAll`, `ManifestDigest`). HEAD each manifest. Unresolvable
   *and* still listed on a fresh tag-list fetch →
   `MarkDue(repo, tag, "dangling")`, which upserts, so no row is
   needed. Gone on re-list means a mid-run delete: skip by count.
   Placement is open (see below).
2. **`reap dangling` policy (tracked rows).** A selector HEADs each
   tracked row's manifest by tag. A 404 or unusable digest produces
   the same mark, idempotent on re-mark, accumulating, included in
   `reap all`. Cost is one HEAD per tracked row.
3. **gc dead-link pass (resolution).** Inside the `kpr gc` flow on
   the parent side, which owns store, lock, and events: walk tag
   link dirs under the already-resolved store root, remove the ones
   with missing revisions, and `UnmarkDue` the matching marks. Runs
   under gc's lock, sentinel events, and dry-run default. Pure
   filesystem work — the collector binary is untouched.

### Lifecycle

Scan or policy marks → the sweeper deletes the dead tag by tag
(202, online) → plan empty. Against a stopped registry the gc
dead-link pass removes the link from disk and clears the mark.
A re-push self-heals earlier via newer-wins `Record`. Dry-run
default throughout.

### Steps

1. **Scanner** + `httptest` unit tests (`dangling`/`skipped` summary
   counts).
2. **`reap dangling` selector** + unit tests (resolvable untouched,
   404 marked, 500 skipped-unmarked).
3. **gc dead-link pass** + unit tests on a `t.TempDir` store layout
   (dead removed and unmarked, live untouched, dry-run removes
   nothing).
4. **Docs + e2e**: ARCHITECTURE.md, then break-link → scan marks →
   sweep fails loud → gc clears → plan empty.

Every other class in the table needs its own detector design —
the walks differ (tag links, manifest blobs, index children,
layer data) and so do the costs. The tag pass does not
generalize; that is why this reads bigger than one plan.

## Risks and open questions

- **Risk: the mark must be definitive, not weather.** Only a 404 or
  a missing/unparseable digest on an otherwise-healthy registry
  marks. Timeouts, 5xx, and refused connections skip with a count.
  A registry mid-restart 404s everything briefly, which is
  indistinguishable from real breakage in one pass — the
  plan-review gate (dry-run default, a human reading `kpr plan`)
  absorbs the false positive.
- **Open: scan placement.** Standalone command, backfill flag, or
  reap-side full pass. Decide at implementation; no new enumeration
  machinery either way.
- **Non-goals:** online GC. (Digest-less sweep support was listed
  here on the 405 belief; live probes show tag deletes succeed, so
  that objection is gone.)
