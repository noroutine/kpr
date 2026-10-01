## Contents

- [Garbage collection today](#garbage-collection-today)
- [Per-repo collection: keep-N over gen tags](#per-repo-collection-keep-n-over-gen-tags)
- [Future: dangling tags](#future-dangling-tags)
- [Future: token-auth registries](#future-token-auth-registries)
- [Risks / Open Questions](#risks--open-questions)

## Garbage collection today

Manifest deletes drop references only; blob bytes need the stock collector against the shared store. `kpr gc` shells the stock `registry garbage-collect` (COPYd from the same `registry:3` the stack runs) after proving, in order: binary/config mounts exist, the store root is local filesystem, the blobdescriptor cache answers (`REGISTRY_REDIS_PASSWORD` — without it the mark phase eats live layers), the sentinel sees a classifiable mode, and the local mount is the registry's own store. Full spec lives in `docs/ARCHITECTURE.md` ("Garbage collection"); this file tracks gc-adjacent future work.

Current behavior in short:

- **Offline.** The collector needs the registry stopped or readonly; kpr proves the mode via sentinel (cancelled blob-upload initiate under a probe repo — 202 writable, 405 maintenance readonly) and refuses anything else.
- **Same-store proof per run, one tracked row.** Every run writes a fresh `noroutine/kpr-sentinel:latest` generation to the local mount — linked at its uuid tag beside the floater — and reads it back through the API: both modes, an empty redis proves fine. The verified mint records one row (gen tag, digest, writer); the floater is never tracked. A real run on writable refuses unless `--force`. Overflow generations die by keep-N (below), not by `--delete-untagged` runs.
- **Advisory lock.** The shared `kpr:gc:lock` (30m bound) serializes kpr-driven runs; distribution's `MarkAndSweep` sets none, so never run a manual `garbage-collect` alongside.
- **Evented runner.** The collector streams through a subprocess with pipe capture, line streaming, and drain discipline; pre/post sentinel events; a mode flip mid-run fails loudly unless `--force`.
- **Dry-run default.** `kpr gc` previews; `--no-dry-run` collects for real.
- **Out of scope: Online GC.** Needs a registry engine; soft-deleted blobs dedupe re-pushes until then.

## Per-repo collection: keep-N over gen tags (done)

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
are the dead-link pass's job, below). Per-run cost stays +2 blobs,
+1 tag, +1 row; steady state is ten tagged generations. Full design
in `docs/SENTINELS.md`.

## Future: dangling tags

A dangling tag is a tag link (`<root>/…/repositories/<repo>/_manifests/tags/<tag>/current/link`) whose manifest revision is gone — crashed delete, backend inconsistency. The tag keeps showing up in listings, lying; pulls fail. Backfill skips unresolvable tags with a count; this plan owns them.

Neither existing tool owns them today:

- The sweeper cannot delete via API: no digest → tag fallback → 405 on distribution:3, stays due by design (`internal/sweep/sweep.go`: "Digest-less rows fall back to the tag and fail visibly").
- Distribution's own collector is no rescue: mark-and-sweep walks manifest revisions, never tag links, so a dead link survives collection untouched. But our `kpr gc` flow — which already resolves the store root, takes the lock, and runs offline — can take the job: a dead-link pass beside the collector.
- reap only knows what redis knows — never-tracked dangling tags are invisible to every policy until something enumerates the registry.

Design: two detectors, one mark kind, gc resolves.

1. **Scanner (full-repo pass).** Enumerate (`_catalog` paging, per-repo tag lists) reusing the backfill registry client (`CatalogAll`, `ManifestDigest`); HEAD each manifest; unresolvable + still listed on a fresh tag-list fetch → `MarkDue(repo, tag, "dangling")` (upserts — no row needed). Gone on re-list → mid-run delete, skip by count. Placement open: standalone `kpr housekeeping` pass, backfill flag, or reap-side full pass.
2. **`reap dangling` policy (tracked rows).** A selector HEADs each tracked row's manifest by tag — 404 or unusable digest → the same mark (idempotent re-mark), accumulating, included in `reap all` (cost: one HEAD per tracked row).
3. **gc dead-link pass (resolution).** In the `kpr gc` flow, parent side (owns store + lock + events): walk tag link dirs under the already-resolved store root, remove ones with missing revisions, `UnmarkDue` the matching marks — under gc's lock, sentinel events, and dry-run default. Pure filesystem work; the collector binary is untouched.

Lifecycle: scan/policy marks → sweeper fails the mark loudly every run (405, stays due — visible, never silent) → `gc --no-dry-run` removes the dead link and clears the mark → plan empty. A re-push self-heals earlier via newer-wins `Record`. Dry-run default throughout.

Steps:

1. **Scanner** + `httptest` unit tests (`dangling`/`skipped` summary counts).
2. **`reap dangling` selector** + unit tests (resolvable untouched, 404 marked, 500 skipped-unmarked).
3. **gc dead-link pass** + unit tests on a `t.TempDir` store layout (dead removed + unmarked, live untouched, dry-run removes nothing).
4. **Docs + E2E**: ARCHITECTURE.md; break-link → scan marks → sweep fails loud → gc clears → plan empty.

## Future: token-auth registries

kpr is scoped to anonymous/basic registries today; against a
token-issuing registry every kpr-owned call (enumeration, sentinel,
proofs) 401s. The future fix is client-side only — kpr never verifies
JWT: present the same user+password pair to the issuer realm from the
401 `Bearer` challenge, fetch per-scope tokens (`registry:catalog:*`,
per-repo `pull`), retry with `Bearer`. One credential form for both
schemes; the stock collector binary is unaffected (it takes its own
auth from registry config). Sharp edge: the sentinel classifies mode
via upload-initiate, which needs a *push*-scoped token on the probe
repo — without it, classification must refuse rather than guess. Loud
refusal when the issuer won't grant catalog scope.

## Risks / Open Questions

- **Risk: the mark must be definitive, not weather.** Only 404 / missing-unparseable-digest on an otherwise-healthy registry marks; timeouts, 5xx, refused connections skip with a count. A registry mid-restart 404s everything briefly — indistinguishable from real breakage in one pass; the plan-review gate (dry-run default, human reads `kpr plan`) absorbs the false positive.
- **Open: scan placement.** Standalone command vs backfill flag vs reap-side full pass — decide at implementation; no new enumeration machinery either way.
- **Known: refused runs still mint.** The proof (mint + verify + row) runs before the writable-without-force refusal gate — pre-existing ordering, so a refused `kpr gc` leaves one tracked generation. Bounded by keep-N (it ages out past ten), not silent; moving the gate ahead of the proof is a follow-up, not this milestone.
- **Non-goals:** digest-less sweep support (the API can't do it — gc's filesystem pass is the answer), Online GC.
