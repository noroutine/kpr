# kpr as registry gateway (spike)

Status: spike branch. Identity shift accepted — kpr grows from
companion sidecar into the registry control plane: edge proxy,
config renderer, process supervisor, eventually multiplexer.
Each layer below is independently useful and unlocks the next;
none is committed beyond the spike (slice 1) until it proves out.

## Thesis

Every hard problem in this project — phantom blobs after gc,
config drift nobody chose, ceremonies done by hand, one
registry per deployment — is a seam between kpr and the
registry it companions. The gateway moves kpr onto the seams:
traffic (proxy), assumption (generated config), lifecycle
(supervisor), placement (multiplexer).

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

## Research: where Location headers come from

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

## Shape

```
push/pull clients ──► kpr:5000 (edge) ──► registry:5000 (private)
kpr internal client ────────────────────► registry:5000 (direct, fence bypass)
```

Compose-wise a port move: kpr takes the edge, the registry
loses its published port. `KPR_REGISTRY_URL` already names the
backend; the new bit is the frontend listener.

## Slice 1 — transparent spike (this branch)

Forward everything, zero policy. Assertions before anything
else:

- Upstream `Location`s are relative (after `relativeurls`).
- Byte-identical large-blob pulls, incl. `Range`/resume and
  broken-pipe parity with direct access.
- Overhead measured; if audible, stop here.

## Later slices (not this branch)

- **Generated config.** kpr renders the registry config it
  already resolves — drift dies, `relativeurls` enforced, and
  the PROOFS.md config-proof future gets its foundation
  (assume what you rendered).
- **Supervisor.** `registry serve` as kpr's child process
  (`--registry=child|external`; external keeps today's sidecar
  working). Kills the control-channel question: stop/collect/
  flush/start become one ceremony with rollback, no socket.
- **Fenced finalize.** gc holds manifest PUTs on a
  self-expiring lease (seconds, fail-open) during final
  verify. Blob uploads never held — they reference nothing.
  kpr-internal traffic bypasses. This *enables* online GC
  (the missing mechanism) without delivering it.
- **Observed tracking.** Receiver rows derived from seen
  manifest PUTs; webhook degrades to corroboration.
- **Multiplexer.** Static namespace→backend routes; same-slice
  mounts fast, cross-slice mounts fail soft to full upload;
  merged `_catalog`; placement of unknown repos as new durable
  state (fenced — the deepest new requirement); per-slice gc
  ceremonies. Dynamic slice API much later.

## Non-goals

Online GC itself (enabled, not delivered), backend
provisioning (buckets, redis instances — the PaaS line),
S3/RESP-compat servers, authn/authz per slice.
