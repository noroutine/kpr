# Backfill — future work

Unbuilt backfill extensions. Shipped behavior lives in
[BACKFILL.md](BACKFILL.md).

## Contents

- [Shadow reader](#shadow-reader)
- [Smaller follow-ups](#smaller-follow-ups)

## Shadow reader

A fallback read path for registries whose `_catalog` backfill
cannot reach.

The gc image already COPYs the stock `registry` binary, and that
same binary can serve as a local read path: run a second copy
against the same shared store and the same redis DB, with
`maintenance.readonly` on, a loopback-only listener, and its own
trivial auth — then point backfill's enumeration at it instead of
the main registry.

**What it buys:** independence from the main registry's access
policy. A blocked `_catalog` and auth-gated namespaces both
disappear without touching main auth or handing kpr credentials.
Reads stay pure (catalog, tag lists, HEADs) against a live view,
readonly mode makes mutation impossible, and sharing the redis DB
keeps blobdescriptor answers consistent.

**Rules:**

- Loopback-only is non-negotiable. An unauthenticated registry must
  never be reachable off-host.
- Config is storage-identical to the main registry, with auth
  replaced.
- It stays an opt-in fallback, never a default — it is one more
  mouth to feed (process, port, config drift), and our own stack
  should simply let kpr reach the main `_catalog` internally.

Not to be confused with `docker-compose.shadow.yml`, which already
exists: that is a *test* overlay producing receiver-blind tags on
demand, not a read path for blocked catalogs.

## Smaller follow-ups

- **Digest-less enrichment.** Backfill fills absence only today; a
  tracked row missing its digest stays missing.
- **GET-with-body-discard fallback** for registries that answer
  HEAD with 405. Possible follow-up, deliberately not in v1.
