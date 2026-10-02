# kpr as registry gateway (spike)

Status: spike branch. Identity shift accepted — kpr grows from
companion sidecar into the registry control plane: edge proxy,
config renderer, process supervisor. Each layer below is
independently useful and unlocks the next; none is committed
beyond the spike (slices 1–2) until each proves out. Multiple
registries under one kpr (multiplexer) are explicitly out —
their own spike, later. Downstream is filesystem-store only;
s3-backed registries are out for the same reason. Mode scope:
`--registry=external` — kpr proxies a separately-run registry.
The child/supervisor mode is a later spike, not this branch.

## Thesis

Every hard problem in this project — phantom blobs after gc,
config drift nobody chose, ceremonies done by hand, one
registry per deployment — is a seam between kpr and the
registry it companions. The gateway moves kpr onto the seams:
traffic (proxy), assumption (generated config), lifecycle
(supervisor), placement (multiplexer).

## What drove this

Two gc scars. First, the collector demands the registry
stopped or readonly — and readonly isn't enough: it's a config
kpr trusts but can't enforce, the flip-restart-flip-back is
manual downtime, and the hot process keeps serving stale blob
descriptors after the collect (HEAD 200 for deleted blobs —
probes and re-pushes mint dead tags off the lie). Second, that
stale cache: gc deletes files but never invalidates
descriptors, and registry:3 offers no flush API.

The gateway answers both without demanding a readonly
registry: fencing (HOLD for finalize, DENY for lock) at the
proxy replaces the readonly flip with edge enforcement, and
the cache is handled by config, not ceremony (no descriptor
cache on the file stack; restart/flush matrix for cached
deployments — full story in `docs/GC.md`; the deferred
redis-alike design space in `docs/BLOBCACHE.md`).

## Layer decisions

**Not the storage layer.** Proxying S3-compat + RESP under the
registry sees bytes, but gc safety reasons about *references* —
and only manifest PUTs create references. Reconstructing that
from object keys is fragile across registry versions, and the S3
surface (multipart, listing semantics) puts corruption risk in
the load-bearing path. RESP buys nothing on top: cache ops
aren't references, and FLUSHDB-after-gc already covers the one
real job. Rejected.

**The API layer.** A reverse proxy in front of `:5000` sees a
manifest PUT as one HTTP call with the reference set in the
body — no reconstruction, protocol kpr already speaks,
stall-the-PUT trivially implementable. Fencing, firsthand
tracking, and verb-level readonly enforcement all live here.

## Shape

```
push/pull clients ──► kpr:5000 (edge) ──► registry:5000 (private)
kpr internal client ────────────────────► registry:5000 (direct, fence bypass)
```

Compose-wise a port move: kpr takes the edge, the registry
loses its published port. `KPR_REGISTRY_URL` already names the
backend; the new bit is the frontend listener.

## Slice 1 — transparent spike (this branch)

Forward everything, zero policy. DELETEs pass straight through
mid-flight — the sweeper stays the sole owner of delete
operations, no extra processing no matter how tempting. Write
proof semantics are unaffected: the proxy forwards bytes, it
neither mints nor alters proofs.

Consequences of the Location research (appendix) for this
slice: with default config the backend emits absolute
`Location`s derived from the incoming request — behind a
`ReverseProxy` that names the unreachable backend, so every
write flow breaks, not just the fence. Flip `relativeurls: true`
by hand now (generation enforces it later), and keep a standing
guard in the proxy asserting every upstream `Location` is
relative or edge-addressed. Test all five `Location`-bearing
flows, not just pulls: upload initiate, chunk resume, mount,
manifest PUT, upload complete. Edge stays at root — subpath
mounting would shift the builder's base path.

The flip earns the spike's first config proof (`internal/proof`,
same pattern): a prover reading the mounted registry config kpr
already resolves, minting `RelativeURLs` only when
`http.relativeurls` is true *and* `http.host` is empty (a set
host silently overrides the knob — proven config must exclude
it). The proxy edge takes the proof at
open — no proof, no edge — because an absolute backend
`Location` is a fence bypass, and bypass must not compile.
Until config generation lands the prover reads an
operator-owned file (trust-but-verify); after, it proves what
kpr rendered.

Assertions before anything else:

- Upstream `Location`s are relative (after `relativeurls`;
  research in the appendix).
- Byte-identical large-blob pulls, incl. `Range`/resume and
  broken-pipe parity with direct access.
- Overhead measured; if audible, stop here.

Implemented: `internal/edge` (proxy + Location guard, proof-gated
handler), `RelativeURLs` in `internal/proof`, embedded in `serve`
(`KPR_EDGE_ADDR` bind, `--config` proof source, `KPR_EDGE=false`
opts out — a failed proof or an opt-out closes the edge loudly,
never a boot refusal). Proven live:
byte-identical blob+manifest through the edge, relative
`Location` off the wire, absolute config refused at open, no
audible overhead (50 HEADs: 0.38s direct vs 0.35s via edge).

## Slice 2 — live fencing (this branch)

Two fence modes at the proxy, both identity-blind:

- **HOLD** (gc finalize): delay manifest PUTs on a bounded
  self-expiring lease (5m crash bound — a crashed gc stalls
  pushes that long, no longer), fail-open. Sleepers re-read
  the lease, so early release wakes promptly; a lease gc
  outruns flows unfenced but says `hold_expired` once — never
  silent. Blob uploads never held — they reference nothing. A
  renewing heartbeat is the real answer past this bound;
  future work.
- **DENY** (store locked): refuse manifest PUT/DELETE fast
  while the lock marker is set — loud status, remedy naming
  `store unlock`. Blob uploads and reads pass: uploads alone
  create no references (orphans are gc-reaped); the lock guards
  semantic mutation, not bytes.

`store lock`/`unlock` extend to live fencing: lock engages DENY
at the proxy besides marking the store (kpr's own use cases
keep refusing on the marker as today); unlock's proof ceremony
is unchanged and stays the sole releaser. The proxy reads the
marker per mutating request — local file read, no cache, no
staleness, and deliberately not redis: microseconds against the
milliseconds the forwarded op costs anyway, and no new
load-bearing dependency on the push path (a redis-gated check
fails open into a fence hole or closed into a dark registry).
If the store ever goes s3-backed, the marker stays local or the
proxy TTL-caches it with a stated leak window — decided then,
not now. Enforcement mints nothing: proofs still govern kpr's
own actions; the fence governs everyone else's. Both modes are
identity-blind (route+method, never credentials; internal vs
external told topologically, direct vs proxied) — registry auth
puts no constraint on this slice. Auth headers pass through
opaque; per-identity fencing belongs to the tenancy world, out
of scope.

Implemented: `edge.Gate` (HOLD/DENY, shared lock evaluation,
edge-triggered flip events + ring outcomes), `gc.Fencer` port +
`Options.Fence` (armed collects hold, previews never, failed
fence refuses), `edge.HoldFile` lease wired in `kpr gc` on file
backends (loud warning otherwise). Proven live: locked PUT→423
with remedy, uploads/reads pass, 3s hold→423 on a locked store,
flips in the ring as `kpr-edge`, and the full cycle in-container
(lock→423, unlock→forwarded) against the bind-mounted store.
One live-caught fix:
post-hold requests re-evaluate the marker (no blind forward).

No new Location work here, but slice 1's guard
becomes fence-critical: any absolute backend `Location` is a
fence bypass, not just a breakage. In-flight chunk URLs from
before a DENY need no revocation — they can't create
references alone; the manifest PUT refusal is sufficient.
One evaluation for both: the fence checks the lock
through the same proof-package path the use cases mint from
(same marker, same read, zero drift between kpr's self-gating
and the proxy's fencing) — token discarded, evaluation shared.
Tokens stay in use-case signatures; the fence needs a boolean,
not a mint.

## Later slices (not this branch)

- **Generated config.** kpr renders the registry config it
  already resolves — drift dies, `relativeurls` enforced, and
  the PROOFS.md config-proof future gets its foundation
  (assume what you rendered).
- **Supervisor (later spike, not this branch).** `registry
  serve` as kpr's child process. Kills the control-channel
  question: stop/collect/flush/start become one ceremony with
  rollback, no socket. This branch stays external — the proxy,
  the fence, and control events work against a registry kpr
  doesn't launch.
- **Control events.** Registry control speaks the gc event
  mechanism, nothing new: lifecycle `Event`s on the existing
  `Reporter` port (`internal/gc/collector.go` — `Timed` stages,
  nil-safe `Emit`, same JSON-lines transport), with control
  stages beside the gc ones (spawn/stop/restart,
  fence-hold/fence-release, ceremony phases), and an `Outcome`
  per completed action into the activity ring
  (`FileStore.PushActivity`, what `store status` shows). A
  restart is as visible as a collect, through the same keys.
- **Observed tracking.** Receiver rows derived from seen
  manifest PUTs; webhook degrades to corroboration.

## Non-goals

Online GC itself (enabled, not delivered), backend
provisioning (buckets, redis instances — the PaaS line),
general-purpose S3/RESP-compat servers, authn/authz per slice, multiple
registries under one kpr (multiplexer — own spike, later),
s3-backed downstream registries (redirect flows, driver
mechanics — own spike, later), and the child/supervisor mode
(own spike, later).

## Appendix: where Location headers come from

distribution 3.1.1, `registry/handlers/app.go` (builder choice)
+ `registry/api/v2/urls.go` (builder). Every `Location` the
registry emits (upload initiate, chunk resume, mount, manifest
PUT, upload complete) comes from one `urlBuilder`:

1. `http.host` set → absolute URLs from the configured host,
   always. The bypass hole in pure form. Neither of our configs
   sets it; never point it at the backend.
2. `http.relativeurls: true` → bare route paths. Proxy-proof
   unconditionally — no host anywhere, nothing to rewrite.
3. Default (our configs today) → absolute URLs from the
   request: `Forwarded` > `X-Forwarded-Host/Proto` > `r.Host`.
   Behind a Go `ReverseProxy`, `r.Host` is the *backend* host
   unless preserved — the fence has a hole until the proxy sets
   the forwarded headers or preserves `Host`.

Mitigations, in order: render `relativeurls: true` (config
slice enforces, not documents); proxy asserts every upstream
`Location` is relative or edge-addressed, logging and rewriting
anything else; never configure `http.host` at the backend.
