# kpr — project goals (outline)

kpr ("keeper") is a lightweight companion sidecar for an OCI
distribution registry. It plugs into a plain `distribution` deployment
(running next to it in docker compose) and adds the lifecycle behavior
a bare registry lacks — without becoming Harbor or Nexus.

Status: starter skeleton. The goals below are an outline, not a design.

## 1. What it is

- A single small Go binary that runs beside stock `distribution`
  (`registry:3`) and talks to it over its public API plus its
  notification hooks.
- Drops into an existing docker compose setup: `kpr` + `redis` +
  `registry`, nothing else required.
- `redis` is the only state backend: TTL tracking, retention
  bookkeeping, nothing that needs a real database.

## 2. Inspiration: ttl.sh

- ttl.sh showed the core trick: encode expiry in the image name/tag,
  learn about pushes through the registry's notification endpoint, and
  track expiries in redis until a reaper deletes them.
- kpr adopts the same mechanism (notification receiver + redis +
  background reaper) but as a reusable sidecar for anyone's private
  registry, not a hosted service.

## 3. Two angles

1. **Ephemeral images** — push `repo/image:<ttl>`, kpr deletes the tag
   when the TTL lapses. For CI artifacts, previews, scratch builds.
2. **Lightweight retention cleanups** — declarative lightweight
   policies for long-lived repos: age-based expiry, keep-last-N tags,
   include/exclude by regexp. Dry-run first, delete second.

## 4. Principles

- Small and boring: one binary, one state store (redis), stdlib-first.
- Safe defaults: policies are opt-in, deletions are logged and
  dry-runnable before they are real.
- No registry fork: stock `distribution`, configured — never patched.
- Observable: management console with health/metrics stays from the
  skeleton; every deletion is explainable (which policy, why).

## 5. Non-goals

- Not a registry itself; no image storage, no authn/authz server.
- No enterprise surface: no RBAC UI, no replication, no signing
  infrastructure, no Harbor/Nexus parity.
- No hosted multi-tenant service; single-team self-hosted scope.

## 6. Open questions

- Notification payload differences between distribution v2/v3 APIs.
- Garbage collection story: tag deletes vs blob reclamation
  (`registry garbage-collect` needs coordination / read-only windows).
- TTL encoding format in tag names; limits and validation.
- Redis schema for TTLs, policy cursors, and delete audit log.
- Whether retention policies live in a config file, env, or a small API.
- Multi-registry support: one kpr per registry, or one-to-many?
