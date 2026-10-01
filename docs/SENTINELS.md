## Contents

- [Current situation](#current-situation)
- [What distribution actually serves](#what-distribution-actually-serves)
- [Same-store proof without API writes](#same-store-proof-without-api-writes)
- [Why not a crafted image](#why-not-a-crafted-image)
- [Rejected: blob re-push as write proof](#rejected-blob-re-push-as-write-proof)
- [Dynamic sentinel: plan and progress](#dynamic-sentinel-plan-and-progress)

## Current situation

The gc sentinel does two jobs under one name, and only one of
them needs a write:

- **Write proof (mode).** `POST /v2/noroutine/kpr-gc-probe/blobs/uploads/`
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

Name: `<sentinel>` = `noroutine/kpr-sentinel`, beside the existing
`noroutine/kpr-gc-probe` write-probe repo. Both under the owned
namespace — no collisions with mirrored images. Fixed names are fine —
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

## Rejected: blob re-push as write proof

Idea: classify mode by re-pushing the known sentinel blob
instead of the initiate-and-cancel probe — 201 writable,
405 readonly, zero garbage when the bytes are identical.
Rejected:

- Readonly 405s before any content logic: the upload
  POST/PATCH/PUT/DELETE handlers aren't registered at all
  in readonly mode (`blobUploadDispatcher`, routing-level),
  so known-digest initiates and cross-repo mounts 405
  without comparison. Verified live against `registry:3`.
- Self-defeating lock: rewriting live evidence is only
  safe with no live traffic (stopped/readonly) — exactly
  when pushes 405. kpr's advisory lock doesn't stop
  registry readers, and link files rewrite via
  truncate-and-write, so a concurrent GET can catch a torn
  link. The initiate probe only touches registry-managed
  session dirs it then deletes.
- Preconditions for the same 1-bit signal: a re-push needs
  a crafted generation on disk first and only ever
  replaces the writable branch (readonly still needs
  fs-write + API-read). Initiate needs nothing.

Conceded: identical bytes can't corrupt, and blob mtimes
are unused (backfill reads tag-link mtimes). Not
corruption that kills it — precondition plus blast radius
for zero new signal. The write probe stays a classifier
with no preconditions; identity stays fs-write + API-read.

## Dynamic sentinel: plan and progress

Supersedes the bare-dir proof above: instead of deleting
the evidence, kpr keeps a live sentinel image — a real
manifest + tag whose config blob carries structured info
(generation, timestamp, writer). Same-store proof becomes
"the store serves the generation I just wrote"; the same
object doubles as a snapshot marker (store shared but
content old = stale snapshot, the backfill blind spot) and
a generic diagnostic vehicle.

Shape (fixed): repo `noroutine/kpr-sentinel`, floater tag `latest`
(the policy-spared name — keep-N runs over this repo with no
excludes and never marks the proof). Every mint links two tags at
the digest: the generation uuid itself (history, keep-N reaps past
ten) and the floater. The minter records one row per generation
(repo, gen tag, digest, push time, writer); the floater is never
tracked. Config blob
= payload JSON `{"v":1,"gen":"<uuid7>","ts":"…","writer":"…"}` —
the generation is time-ordered, so two observed generations
compare without parsing timestamps. Nested under `kpr/` so one
glob excludes every kpr-owned repo from backfill enumeration;
leading-underscore namespaces are out (name components must
start alphanumeric per the distribution-spec grammar, and the
router 404s them — both verified against `registry:3`).
Namespaced under `noroutine/` (owned) rather than the bare
tool name — no collisions with other tenants' repos.
Upgrading from an older address (flat `kpr-sentinel`, then
`kpr/sentinel`): remove the stale repo dir once —
a tagged manifest is kept by the collector forever.
(arbitrary bytes — blobs are never validated). Manifest =
minimal OCI image manifest, `config` pointing at the real
payload digest, payload digest repeated in
`annotations{kpr.sentinel:1, kpr.gen:N}` for tag-level
reads. Update = write new blobs + links, atomic rename of the
floater (`tags/latest/current/link`); the gen tag is written once
and never repointed. Past-ten keep-N marks overflow generations
due, the sweep deletes their docs by digest (the registry unlinks
referencing tags server-side — e2e-pinned), and default collects
reap the dangling blobs. Steady state: ten tagged generations of
readable proof history, zero `--delete-untagged` needed.

### M0 spike: hand-crafted round-trip (done)

Throwaway containers in `/tmp/spike` (not committed),
real `registry:3` + `redis:8-alpine`, prod-like config
(redis blobdescriptor cache). Pushed one reference image
via API, copied its on-disk conventions by hand, then
served a fully hand-crafted repo. Findings:

- Hand-written global blob + `_layers` link → `GET blob`
  200. Hand-written revision + tag links → `GET manifest`
  200 with exact bytes, `tags/list` and `_catalog` list it.
- Cache miss falls through to fs: identical results under
  `inmemory` and `redis` blobdescriptor cache. Repointing
  `current/link` serves the new generation immediately —
  manifests are never cached. Redis residue is small
  descriptor keys per served blob (`blobs::…`,
  `repository::<repo>::blobs::…`), no TTL.
- Manifest content must be schema-valid JSON
  (`schemaVersion: 2`, known media type). Garbage bytes →
  GET 500s, and worse: `garbage-collect` **aborts the whole
  mark phase** on an unparseable revision. Every revision
  link under the sentinel repo must stay parseable —
  generation writes are all-or-nothing.
- References are NOT validated on GET (bogus config digest
  serves fine), but the mark phase marks referenced blobs —
  so `config` points at the real payload digest: the
  current payload stays live, old payloads sweep away.
- Drive-by learning: monolithic `PUT` with curl
  `--data-binary "$var"` sends an empty body under
  `application/x-www-form-urlencoded` (the 400s); files or
  explicit `Content-Type: application/octet-stream` work.
  PATCH-then-PUT is the reliable shell flow; kpr never
  pushes this way (fs writes only).

### M1: layout writer, unit-tested (done)

`Write(root, repo, tag, payload)` in a new
`internal/sentinel` package: config blob + manifest blob +
layer/revision/tag links, exact conventions from M0 (link
files with no trailing newline, atomic tag switch via
rename). Tests on `t.TempDir`: file set, digest
correctness, manifest shape parseable as OCI, repoint
moves the tag. No registry needed. Name validation
follows the OCI tag shape; nested repos allowed, `.`/`..`
refused.

### M2: API reader, stub + e2e (done)

`Read` returns the served payload plus the manifest
digest; `Verify` compares the generation. Port: minimal
`API` interface (`GetManifest`, `GetBlob`); second
implementation per W2 is the `internal/registry` client
extended with Accept-header GETs (bare `get` 406s on
manifests). Unit tests against `httptest` stubs plus a
hand stub behind the port; e2e
(`test/e2e/sentinel_dynamic_test.go`) against `registry:3`
with a bind-mounted store — write layout, read via API,
bump the generation, verify the repoint serves fresh.
Absence (read before write) refuses.

### M3: gc same-store proof on sentinel (done)

`gc Run` writes a fresh `noroutine/kpr-sentinel:latest` generation per
armed run and reads it back through the API — same proof both
modes, an empty redis proves fine; dry-run previews skip the proof
and record nothing. `Run`
dropped the old identity ports (`FirstDigestRow`,
`SameStoreUpload`/`SameStoreTagLink` — deleted with
their tests); it keeps `Probe` (mode), `Locker`, and
`Collector`, gains the `sentinel.API` port the registry
client carries, and a narrow `Recorder` (one `Record`
method) the store backends satisfy. Unit tests run behind a
file-backed fake registry (`fileAPI` over the staged
root) plus a frozen-generation fake for the stale case;
cli tests serve sentinel files from disk over httptest.
Stranger store and stale snapshot both refuse with "does
not share", before the collector spawns. The write probe
keeps classifying mode only.

### Tagged generations, keep-N reaped (done)

Every mint links its uuid as a tag beside the floater and, after a
verified read-back, records one row (repo, gen tag, digest, push
time, writer) — never a row for the floater. keep-N marks all but
the ten freshest; the sweep deletes overflow docs by digest and
drops the rows; default collects reap the dangling blobs. Pinned
end to end (`test/e2e/sentinel_keepn_test.go`): twelve mints → two
marked `keep-n:exceeds 10` → swept with zero failures → old docs
404, ten survivors serve, the floater serves the newest, no floater
row exists. Per-run residue is +2 blobs, +1 tag, +1 row; steady
state is ten tagged generations. A mint that fails to record
refuses ("proof held but the generation went untracked") — an
untracked gen is a permanent tag, never silent litter. The
`--delete-untagged` flows stay untouched; this route never needs
them for sentinel history.

## Locality: shared store unlocks, its absence degrades

Locality means access to the same storage the registry serves —
the filesystem mount today, S3/Azure/GCS bucket access tomorrow.
It is a capability key, not a topology flag: nothing declares
local or remote, and no mode is cached. Every operation that
touches store bytes proves first (fresh mint + read-back, gc-style)
and refuses unproven; everything else runs anywhere and never
notices.

What locality gates (bytes, not names):

- blob collection (the stock collector runs on the store);
- generation minting (proof writes land on the store);
- storage accounting, deep backfill verify, orphaned upload dirs,
  blob pinning — anything that must see bytes, not names.

What works degraded (names over the API, no proof needed):

- event intake, TTL rows, manifest sweeps, keep-N over known
  tags, catalog backfill, writability probing, the console.

The degraded failure mode is silent blob accumulation (tags go,
layers stay), so staleness is loud, never gating: the console
proof card shows the live generation and its age, serve logs the
proof age at boot, the sweep loop voices it at startup and on
fresh↔stale transitions (`ProofStaleAfter`, 7 days). A stale or
missing proof warns; it never refuses a sweep.

Enforcement rule: only locality-gated operations touch store
bytes, and each mutating one proves first — today that is one
call site (`gc.Run` armed); dry-run previews are reads, unproven
by design. Storage accounting and friends take the same
mint+verify preamble when they arrive, never inherited trust.

## The lock: default-deny intent (`lock` / `unlock`)

Proof is locality; the marker is intent. `kpr unlock` mints a
fresh generation, verifies the read-back, and only then records
`kpr:store:unlocked` (redis key, `<dir>/unlocked` file) —
unlock on a stranger's store refuses and the marker stays down.
`kpr lock` drops the marker. Fresh stores read locked: `gc`
refuses before probing, with the fix named. Reads, sweeps, and
the receiver never check the marker — locked behaves exactly
like no shared store for write ops, which makes `lock` the
remote-mode simulator for tests. The console store card voices
`locked` alongside reachability.
The seam already points at object stores: `Write` takes the
location (fs root now, bucket+prefix later behind a writer
port) while `Read`/`Verify` go through the registry API and stay
identical.

### M4: backfill snapshot detection (open)

Backfill reads the sentinel via API and compares
generation/timestamp against expectations: shared store
with an old snapshot becomes visible instead of silently
trusted. Ground laid: `Verify` refuses typed —
`sentinel.Mismatch` (answered, wrong generation) vs plain
read error (no evidence) — noted in `docs/BACKFILL.md`.
Payload schema and staleness policy decided here, not
earlier.
