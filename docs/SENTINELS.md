# Sentinels: proof the store is ours

The gc sentinel answers one question — is the store kpr sees the
store the registry serves — in both modes (writable and
maintenance readonly), with no tracked rows and no API writes.
A live sentinel image doubles as a snapshot marker (shared store
but old content = stale snapshot) and a generic diagnostic
vehicle.

## Contents

- [Registry facts the design leans on](#registry-facts-the-design-leans-on)
- [Why not a crafted image](#why-not-a-crafted-image)
- [Rejected: blob re-push as write proof](#rejected-blob-re-push-as-write-proof)
- [The sentinel today](#the-sentinel-today)
- [Locality: shared store unlocks, its absence degrades](#locality-shared-store-unlocks-its-absence-degrades)
- [The lock: default-deny intent (`lock` / `unlock`)](#the-lock-default-deny-intent-lock--unlock)
- [Lineage: whose registry is this (`id`, verdicts, `kpr store adopt`)](#lineage-whose-registry-is-this-id-verdicts-kpr-store-adopt)
- [Future: backfill snapshot detection](#future-backfill-snapshot-detection)

## Registry facts the design leans on

Studied against distribution/distribution HEAD (registry:3
lineage), filesystem driver. No reverse proxy, no new
endpoints — only what the registry already exposes:

- **No static serving outside `/v2/`.** Anything else 404s. A
  blob dropped on disk is reachable only through the blob API,
  which demands a per-repo `_layers` link first (missing link
  → `ErrBlobUnknown`, no fallback to the global store).
- **Manifests are never cached.** Repointing `current/link`
  serves the new generation immediately (verified live under
  both `inmemory` and `redis` blobdescriptor cache).
- **Every revision link must stay schema-valid JSON.**
  Garbage bytes → GET 500s, and worse: `garbage-collect`
  **aborts the whole mark phase** on an unparseable revision.
  Generation writes are all-or-nothing.
- **References are NOT validated on GET** (a bogus config
  digest serves fine), but the mark phase marks referenced
  blobs — so `config` points at the real payload digest: the
  current payload stays live, old payloads sweep away.
- Blob GETs populate the redis blobdescriptor cache (`HMSet`,
  no `EXPIRE` — entries never time out). Reads that avoid
  blob GETs leave no residue.

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
  in readonly mode (routing-level), so known-digest initiates
  and cross-repo mounts 405 without comparison. Verified
  live against `registry:3`.
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

The write probe stays a classifier with no preconditions;
identity stays fs-write + API-read.

## The sentinel today

Repo `noroutine/kpr-sentinel`, floater tag `latest` (the
policy-spared name — keep-N runs over this repo with no
excludes and never marks the proof). Every mint links two
tags at the digest: the generation uuid itself (history,
keep-N reaps past ten) and the floater. The minter records
one row per generation (repo, gen tag, digest, push time,
writer); the floater is never tracked. Config blob
= payload JSON `{"v":1,"gen":"<uuid7>","id":"<uuid7>","ts":"…","writer":"…"}` —
the generation is time-ordered, so two observed generations
compare without parsing timestamps. Nested under `kpr/` so one
glob excludes every kpr-owned repo from backfill enumeration;
leading-underscore namespaces are out (name components must
start alphanumeric per the distribution-spec grammar, and the
router 404s them — both verified against `registry:3`).
Namespaced under `noroutine/` (owned) rather than the bare
tool name — no collisions with other tenants' repos.
Manifest = minimal OCI image manifest, `config` pointing at the real
payload digest, payload digest repeated in
`annotations{kpr.sentinel:1, kpr.gen:N}` for tag-level
reads. Update = write new blobs + links, atomic rename of the
floater (`tags/latest/current/link`); the gen tag is written once
and never repointed. Past-ten keep-N marks overflow generations
due, the sweep deletes their docs by digest (the registry unlinks
referencing tags server-side — e2e-pinned), and default collects
reap the dangling blobs. Steady state: ten tagged generations of
readable proof history, zero `--delete-untagged` needed.

Per-run cost is +2 blobs, +1 tag, +1 row. A mint that fails
to record refuses ("proof held but the generation went
untracked") — an untracked gen is a permanent tag, never
silent litter. The `--delete-untagged` flows stay untouched;
this route never needs them for sentinel history.

Pinned end to end (`test/e2e/sentinel_dynamic_test.go`,
`test/e2e/sentinel_keepn_test.go`): write layout → API read →
repoint serves fresh; twelve mints → two marked
`keep-n:exceeds 10` → swept with zero failures → old docs
404, ten survivors serve, the floater serves the newest, no
floater row exists.

Mode classification (not identity) stays the write probe:
`POST /v2/noroutine/kpr-gc-probe/blobs/uploads/`
(`internal/gc/sentinel.go`) — 202 means the registry takes
writes, 405 means v3 maintenance readonly. The upload is
cancelled at once, leaving nothing.

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

- event intake, TTL rows, keep-N over known tags, catalog
  backfill, writability probing, the console. Manifest sweeps are
  NOT degraded work: the sweeper gates every pass on lineage
  (below) and refuses foreign, silent, or rolled-back registries
  before its lock, let alone any delete.

The degraded failure mode is silent blob accumulation (tags go,
layers stay), so proof-age staleness is loud, never gating: the
console proof card shows the live generation and its age, serve
logs the proof age at boot, the sweep loop voices it at startup
and on fresh↔stale transitions (`ProofStaleAfter`, 7 days). A
stale or missing *locality proof* warns; a failed *lineage
verdict* refuses (different proofs — age vs identity).

Enforcement rule: only locality-gated operations touch store
bytes, and each mutating one proves first — today that is one
call site (`gc.Run` armed); dry-run previews are reads gated on
the served generation (presence, not freshness). Storage
accounting and friends take the same mint+verify preamble when
they arrive, never inherited trust.

## The lock: default-deny intent (`lock` / `unlock`)

Proof is locality; the marker is intent. `kpr store unlock` mints a
fresh generation, verifies the read-back, and only then records
`kpr:store:unlocked` (redis key, `<dir>/unlocked` file) —
unlock on a stranger's store refuses and the marker stays down.
`kpr store lock` drops the marker. Fresh stores read locked: `gc`
refuses before probing, with the fix named. Reads, sweeps, and
the receiver never check the marker — locked behaves exactly
like no shared store for write ops, which makes `lock` the
remote-mode simulator for tests. The console store card voices
`locked` alongside reachability.
The seam already points at object stores: `Write` takes the
location (fs root now, bucket+prefix later behind a writer
port) while `Read`/`Verify` go through the registry API and stay
identical.

## Lineage: whose registry is this (`id`, verdicts, `kpr store adopt`)

Locality proves *access*; lineage proves *ownership*. Every
mint carries the store's lineage identity (`id`, a uuid7 minted
beside the first generation); every altering path — `gc`,
`unlock`, the sweeper — reads the served generation first and
judges it against the paired identity plus tracked rows
(`internal/lineage`: pure, no network; the table below mirrors
its cases one to one, e2e-pinned in `test/e2e/lineage_test.go`).
No TOFU, no acceptance re-pairing: a pairing is set by an explicit
ceremony or not at all, and HA means one shared kpr store —
separate stores fork lineages and refuse by design.

| Served | Store | Verdict |
| --- | --- | --- |
| nothing (absent), dry-run | any | refuse — previews cannot establish; run armed first |
| nothing (absent), armed | any | establish — mint the baseline under the stored id (fresh store: generate one) |
| unreadable / unparseable / future timestamp | any | refuse |
| identity-less (pre-pairing) | any | refuse, never auto-adopted — wipe the volume or remove stale tags (even explicit `adopt` won't bless it) |
| identity, store unpaired | — | refuse — `kpr store adopt` pairs, optionally pinned to an expected id |
| foreign identity | paired elsewhere | refuse, no acceptance overrides — `kpr store adopt` re-pairs (prunes the old epoch's sentinel rows) |
| generation older than tracked | paired | refuse armed (`--accept-rollback` if the restore was intentional); warn through dry-run and `--accept-rollback`; proceed clean once accepted via `kpr store adopt --gen` |
| untracked generation, own identity | paired | proceed — adopt-recorded for keep-N (armed runs only) |
| tracked newest | paired | proceed |

Clock: mint timestamps come from a checked clock —
`KPR_TIME_METHOD` `local` (default) / `https` / `ntp`,
`KPR_TIME_SERVER` defaulting to `zeitstempel.dfn.de`, compose
pinning `https`. Full approach in `docs/TIMESTAMPS.md`; in
short: skew past 30s refuses unless `--accept-clock-skew`
(accepted runs warn), an unreachable source warns and proceeds on local
time — air-gapped sites stay working.

Gates, in order per operation: clock check → lock → mode
(writable refuses pre-proof unless cache- and fence-accepted — no point minting a
generation the gate will reject) → lineage verdict → proof mint
(id-bearing) → verify → record → collect. Dry-run reads
presence, not freshness; refused runs mint nothing. `kpr store unlock`
judges through the same verdict: foreign/unpaired-served/
identity-less/stale refuse with the ceremony named (plus a
skewed clock — unlock carries no accept flags); silence
establishes, warning loud under an already-paired store. The sweeper
carries no accept flags and never mints, so silence and stale-armed
refuse there; its refusal is a skipped pass with the cause in
`failures`, before the sweep lock. `kpr store adopt [IDENT] [--gen]`
is the only pairing writer: follow the served id, pin an
expected one (mismatch refuses), accept a rollback baseline
(`--gen` must name the served generation). (This ceremony is
not [Adopting kpr](ADOPT.md) — that guide bolts kpr onto an
existing registry; this one pairs a store to a lineage.)

## Future: backfill snapshot detection

Backfill reads the sentinel via API and compares
generation/timestamp against expectations: shared store
with an old snapshot becomes visible instead of silently
trusted. Ground laid: `Verify` refuses typed —
`sentinel.Mismatch` (answered, wrong generation) vs plain
read error (no evidence) — noted in `docs/BACKFILL.md`.
Payload schema and staleness policy decided here, not
earlier.
