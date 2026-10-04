# Redis-alike blob descriptor cache (deferred)

Question: should kpr speak the registry's descriptor-cache
protocol itself, making redis optional — persistent across
restarts, helping cloud-backed registries? Verdict: not now;
this doc keeps the findings so the decision survives.

## Findings

- Descriptor values are immutable: digest → {mediaType, size}
  is true forever. The only lie is deletion (scar 2), and kpr
  knows deletions firsthand — it runs gc. Every other failure
  (eviction, wipe, total loss) degrades to slowness, never
  wrongness.
- The command surface is closed and tiny: whatever the pinned
  registry version emits for descriptors, nothing else.
  Anything outside gets a loud refusal; version skew shows up
  in testing, same as the collector binary pin.
- One property still unverified and load-bearing: that
  distribution fails *open* to backend on cache errors. That
  decides the safety story; confirm from source before any
  implementation.
- A test-double substrate (miniredis) is production clothing
  it can't afford: no eviction, unbounded growth, nothing a
  load-bearing path should stand on.

## Why kpr-dir backing fails

Backing the cache with files under kpr's dir works today and
breaks the day kpr's dir goes cloud object store: the cache
would then be as slow as (or slower than) the storage calls it
exists to skip, while adding a protocol-compat burden for the
privilege. A cache must be strictly faster than its backend on
every backing change, or it is a tax with a staleness hazard.

## The constraint

A redis-alike blobcache earns its existence iff it buys both:

1. speedup over registry storage calls, on every backend it
   fronts, and
2. persistence across restarts (warm restarts are the whole
   point for object-store deployments).

That limits the design space to three shapes:

- **Redis proxy.** kpr forwards the descriptor protocol to
  real redis, observing passively; invalidation hooks ride
  along. No compat burden beyond forwarding, failure mode is
  bypass.
- **Redis bridge to local filesystem.** kpr implements the
  descriptor subset against local disk — ephemeral-friendly,
  restart-cold by design. Only where local disk is strictly
  faster than the registry backend (never against kpr-dir
  futures — see above).
- **Narrow proper implementation.** A real, persistent,
  bounded-memory descriptor store speaking the subset — only
  if clustering (or warm restarts without redis to operate)
  ever demands it. Descriptor-only endpoint, wipe-and-cold-
  start recovery, gc-victim invalidation closed-loop in the
  ceremony.

## Decision

Filesystem deployments: no cache (proven — [`docs/GC.md`](GC.md)).
Cloud-backed deployments: real redis, flushed/DEL'd by the
ceremony. Nothing built until a deployment needs warm restarts
without a redis to operate.
