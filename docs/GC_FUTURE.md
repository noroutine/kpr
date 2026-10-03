# Garbage collection — future work

Designs for gc work that is **not built**. Shipped behavior lives in
[GC.md](GC.md).

## Contents

- [Per-repo collection](#per-repo-collection)
- [Dangling tags](#dangling-tags)
- [Token-auth registries](#token-auth-registries)
- [Risks and open questions](#risks-and-open-questions)

## Per-repo collection

The stock collector marks globally: every repository in the store,
every run. There is no way to tell it "collect only this repo",
and kpr inherits that — `kpr gc` has no scoping flag because
there is nothing to pass one to.

This matters most for blast radius. An operator who wants to
reclaim space from one noisy CI namespace has to collect
everything, which means the whole store pays the mark-phase cost
and the whole store is exposed to whatever the run gets wrong.

One repo already works around it, and the workaround is the clue
to a general design: kpr's own sentinel repo keeps its history
bounded without gc scoping at all. Each generation carries a
tracked row, keep-N marks the overflow, the sweeper deletes those
manifests by digest, and an ordinary global collect reclaims the
blobs that are now unreferenced. Tag-level retention does the
scoping; gc stays global and dumb. See [SENTINELS.md](SENTINELS.md).

Whether that generalises — per-repo retention driving a global
collect, rather than a scoped collect — is the open question. A
genuinely scoped collect would need our own mark phase, which is
the same objection that keeps online GC out.

## Dangling tags

A dangling tag is a tag link whose manifest revision is gone —
a crashed delete, or backend inconsistency:

```
<root>/…/repositories/<repo>/_manifests/tags/<tag>/current/link
```

The tag keeps showing up in listings, lying, and pulls fail.
Backfill skips unresolvable tags with a count; this plan owns them.

### Why no existing tool owns them

- **The sweeper can't.** No digest means a tag fallback, which 405s
  on distribution:3. The row stays due by design
  (`internal/sweep/sweep.go`: "Digest-less rows fall back to the tag
  and fail visibly").
- **The stock collector can't.** Mark-and-sweep walks manifest
  revisions, never tag links, so a dead link survives collection
  untouched.
- **`reap` can't.** It only knows what the store knows, and
  never-tracked dangling tags are invisible to every policy until
  something enumerates the registry.

But the `kpr gc` flow already resolves the store root, takes the
lock, and runs offline — so it can take the job as a dead-link pass
beside the collector.

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

Scan or policy marks → the sweeper fails the mark loudly every run
(405, stays due, visible rather than silent) → `gc --no-dry-run`
removes the dead link and clears the mark → plan empty. A re-push
self-heals earlier via newer-wins `Record`. Dry-run default
throughout.

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

## Token-auth registries

kpr is scoped to anonymous and basic registries today. Against a
token-issuing registry, every kpr-owned call — enumeration,
sentinel, proofs — 401s.

The fix is client-side only; kpr never verifies JWT:

1. Read the issuer realm from the 401 `Bearer` challenge.
2. Present the same user+password pair to it.
3. Fetch per-scope tokens (`registry:catalog:*`, per-repo `pull`).
4. Retry with `Bearer`.

One credential form covers both schemes, and the stock collector
binary is unaffected — it takes its own auth from registry config.

**Sharp edge:** the sentinel classifies mode via upload-initiate,
which needs a *push*-scoped token on the probe repo. Without one,
classification must refuse rather than guess. Likewise a loud
refusal when the issuer won't grant catalog scope.

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
- **Non-goals:** digest-less sweep support (the API can't do it —
  gc's filesystem pass is the answer), and online GC.
