## Contents

- [Garbage collection today](#garbage-collection-today)
- [After an armed run: stale blob descriptors](#after-an-armed-run-stale-blob-descriptors)
- [Per-repo collection: keep-N over gen tags](#per-repo-collection-keep-n-over-gen-tags)
- [Future: dangling tags](#future-dangling-tags)
- [Future: token-auth registries](#future-token-auth-registries)
- [Risks / Open Questions](#risks--open-questions)

## Garbage collection today

Manifest deletes drop references only; blob bytes need the stock collector against the shared store. `kpr gc` shells the stock `registry garbage-collect` (COPYd from the same `registry:3` the stack runs) after proving, in order: binary/config mounts exist, the store root is local filesystem, the sentinel sees a classifiable mode (readonly: the blobdescriptor cache answers — `REGISTRY_REDIS_PASSWORD`, without it the mark phase eats live layers; writable: the online preflight below), and the local mount is the registry's own store. Full spec lives in `docs/ARCHITECTURE.md` ("Garbage collection"); this file tracks gc-adjacent future work.

Current behavior in short:

- **Offline.** The collector needs the registry stopped or readonly; kpr proves the mode via sentinel (cancelled blob-upload initiate under a probe repo — 202 writable, 405 maintenance readonly) and refuses anything else.
- **Online.** A serving registry collects under the gateway fence: the preflight clears the blob cache (none configured — with a redis cache, deletes stay vouched until restart) and the gateway (proven edge listening, HOLD lease configured), armed refuses on unaccepted misses with every miss listed at once, dry-run prints the checklist and previews on. The collect engages the HOLD lease around finalize; a fence that fails to engage refuses instead of collecting unfenced. Overrides: `--accept-blob-cache`, `--accept-unfenced` — each names the risk it accepts, on an armed run only.
- **Same-store proof per armed run, one tracked row.** Every armed run writes a fresh `noroutine/kpr-sentinel:latest` generation to the local mount — linked at its uuid tag beside the floater — and reads it back through the API: both modes, an empty redis proves fine. The mint carries the store's lineage id; the served generation is judged against the paired identity first (verdict table in `docs/SENTINELS.md` — foreign refuses even forced, stale refuses armed, silence establishes). The verified mint records one row (gen tag, digest, writer); the floater is never tracked. Dry-run previews read instead of minting (above). A real run on writable clears the online preflight first (above), before any mint. A clock check opens every run: skew past 30s refuses unless forced, an unreachable source warns and proceeds (`KPR_TIME_METHOD` local/https/ntp defaulting to local, `KPR_TIME_SERVER` defaulting to `zeitstempel.dfn.de`; compose pins https — full approach in `docs/TIMESTAMPS.md`). Refused pairings heal through `kpr store adopt`, never `--force`. Overflow generations die by keep-N (below), not by `--delete-untagged` runs.
- **Advisory lock.** The shared `kpr:gc:lock` (30m bound) serializes kpr-driven runs; distribution's `MarkAndSweep` sets none, so never run a manual `garbage-collect` alongside.
- **Evented runner.** The collector streams through a subprocess with pipe capture, line streaming, and drain discipline; pre/post sentinel events; a mode flip mid-run fails loudly unless `--force`.
- **Dry-run default.** `kpr gc` previews; `--no-dry-run` collects for real. Previews never mint — no blobs, no tags, no rows — but they still read: the served generation proves presence (nothing served refuses with "no sentinel served" and names the armed ceremony; unreadable stays an error). Freshness stays armed-only: only a fresh mint distinguishes a stale snapshot from the shared store. Only armed runs pay for (and print) the proof.
- **Online, not offline-first.** The preflight derives the path from the serving probe — stopped collects classic, serving collects fenced. There is no `--online` flag to forget: the mode decides, the checklist explains.

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
- **Non-goals:** digest-less sweep support (the API can't do it — gc's filesystem pass is the answer), Online GC.
