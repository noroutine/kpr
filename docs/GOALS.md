# kpr — project goals

kpr ("keeper") fronts a stock OCI `distribution` registry:
ephemeral images plus lightweight retention cleanups, without the
weight of Harbor or Nexus.
Inspired by ttl.sh. The design lives in `docs/ARCHITECTURE.md`;
this page is the backdrop — what it is, what it won't become,
and what's still open.

## What it is

- A single small Go binary in front of stock `distribution`
  (`registry:3`). Pushes land on kpr's edge and forward
  byte-identical; kpr also talks to the registry over its public
  API and notification hooks. No registry fork: stock, configured
  — never patched. The edge is default-on and `KPR_EDGE=false`
  turns it off, leaving kpr fully out of the data path.
- Drops into an existing compose setup: `kpr` + state +
  `registry`, nothing else required. State is plain files by
  default; redis is opt-in (`KPR_REDIS_ADDR`).
- Two angles:
  1. **Ephemeral images** — push `repo/image:<ttl>`, kpr deletes
     the tag when the TTL lapses. CI artifacts, previews,
     scratch builds.
  2. **Lightweight retention cleanups** — small declarative
     policies for long-lived repos (age expiry, keep-last-N,
     include/exclude). Dry-run first, delete second.

## Principles

- Small and boring: one binary, one state store, stdlib-first.
  Opinions written as plain code — simple use-case driven policies.
- Safe defaults: policies opt-in, deletions logged and
  dry-runnable before real. Loud refusals, never silent runs.
- Observable: console with health/metrics; every deletion
  explainable (which policy, why).

## Non-goals

- Not a registry itself; no image storage, no authn/authz server.
- No enterprise surface: no RBAC UI, no replication, no signing
  infrastructure, no Harbor/Nexus parity.
- No hosted multi-tenant service; single-team self-hosted scope.
- No policy/workflow engine, scheduler, per-repo rule sets,
  cloud integrations, online registry GC.

## Open questions

- Multi-registry support: one kpr per registry, or one-to-many?
- Token-auth registries: same credential pair exchanged at the
  issuer per scope (client-side only — kpr never verifies JWT).
  Design sketched in `docs/GC.md`; unbuilt.
- Digest-less row enrichment: `kpr store backfill` fills absent
  rows only (`docs/BACKFILL_FUTURE.md`).
- Detached operation over the console HTTP surface (the CLI
  talks to state directly today and runs colocated).
- Dangling tag links (dead links from crashed deletes): design
  in `docs/GC.md`; unbuilt.
