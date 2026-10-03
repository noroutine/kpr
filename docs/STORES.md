## Contents

- [The three stores](#the-three-stores)
- [FileStore layout](#filestore-layout)
- [Invariants](#invariants)
- [Wiring: deriving the backend](#wiring-deriving-the-backend)
- [The self-contained backup](#the-self-contained-backup)
- [Limits](#limits)

## The three stores

Every kpr process programs to the `store.Store` port
(`internal/store/store.go`): rows, due marks, run state,
the activity ring, and both single-flight locks. Three
adapters carry it; the `storetest` contract suite runs
identical scenarios against all of them — one contract,
never a copy per backend.

- **RedisStore** (the code default when `KPR_STORE` is unset):
  rows in a `kpr:rows` HASH,
  locks as expiring keys. Multi-process safe by
  construction, server-side TTL expiry on locks. Needs the
  shared redis from the redis compose stack.
- **MemStore**: in-memory, hermetic unit tests. Not for
  production.
- **FileStore** (`KPR_STORE=file`): per-row JSON files
  under one dir, flock'd lock files, no redis. For
  redis-less deploys — a single registry volume carries
  everything (see below). Local volumes only.

Row JSON is byte-identical across backends (plain
`json.Marshal(policy.Row)`), so rows stay portable
whatever carries them.

## FileStore layout

Rooted at `KPR_STORE_DIR`, default `kpr` (relative to the
working directory — run serve and CLI from one place, or
set an absolute path; a split cwd silently forks state,
so compose always sets it absolute (the file stack mounts the
`kpr-data` volume at `KPR_STORE_DIR=/var/lib/kpr`):

- `rows/<repo-path…>/<tag>.json` — one file per row, the
  repo split into subdirs mirroring the registry
  hierarchy (`rows/noroutine/kpr-app/v1.json`). Elements
  are `%`-escaped, `.`/`..`/empty refused. `Record`
  applies the shared newer-wins rule (re-notification
  preserves the due mark, newer push restarts it).
- `current.json` — the one pass record, overwritten.
- `activity.json` — the capped outcome ring (newest
  first, trimmed to `ActivityCap`). One record per due row the
  sweeper attempts: repo, tag, reason, outcome (`deleted` /
  `planned` / `failed` / `untracked`), timestamp. The sweeper is
  the only writer; the console Activity section, the
  performed/planned/failed counters (counted straight from the
  ring), and `kpr store status` (tail of 10, full ring under
  `--json`) are the readers. A journal of outcomes, not a log
  stream — verbose operational logs stay on stdout/OTLP.
- `locks/<name>.lock` — the lock file IS the lock JSON
  (`holder`, `since` for the operator's `cat`). Truth
  stays with the flock, never the file: a claim without
  the lock means nothing.
- `.lock` — serializes mutating ops across processes
  sharing the dir, held for milliseconds per op.
- `unlocked` — the intent marker (empty file, presence is the
  state): `kpr store unlock` creates it after proving the shared
  store, `kpr store lock` removes it. Missing reads locked.
- `identity.json` — the lineage pairing (redis: `kpr:identity`
  key): which registry lineage this store belongs to. Absent
  means unpaired; `kpr store adopt` is the only writer.

`Ping` proves the dir exists *and* writable with a probe
file (banner red, sweeper skips) — a Stat would lie about
read-only mounts.

## What locked means

Locked (marker absent — fresh stores included) means hands off:
no destructive operations against the registry or the store.
No collects, no manifest deletes, no row drops. It does *not*
mean the registry is readonly — writability is enforced only by
registry config (`maintenance.readonly`); the lock never claims
that, and a locked store can still serve reads and previews.

Two aims, from when `adopt` appeared: foreign or stale stores
must never be garbled quietly (unlock proves sharedness first,
and every surprise arrives with its warning and remedy), and the
operator gets a simple maintenance freeze. Reads and plan edits
ignore the marker — nothing destructive, nothing to refuse.
Passes and collects refuse locked even as previews: a run is a
run, not a read.

Enforced where a check exists (`gc` opens via the marker);
threading the sweeper pass and `rm` through the same gate is the
Miss 3 slice.

## Invariants

Crash-proofing is a write protocol, not a format:

1. **All mutations are atomic filesystem ops** — temp-file
   rename, unlink, mkdir. Live files are never opened for
   writing (except the lock claim, written only while its
   flock is held). Readers see old or new, never torn —
   there is no invalid state reachable, hence no recovery
   procedure.
2. **Temp files carry no `.json` suffix** (`.tmp-*`) and
   readers skip non-JSON: an abandoned temp from a
   `kill -9` is invisible, never a phantom row.
3. **Torn `.json` refuses loudly with the path.** Our own
   protocol can never produce one — so one present means
   foreign garbage or disk trouble, and silence would be
   the lie.
4. **Lock files are never unlinked.** A new file would be
   a new lock the old holder doesn't exclude; release is
   close, and the kernel releases on holder death —
   strictly better than TTL expiry (accepted `ttl`
   parameter is ignored).
5. **Per-op atomicity only.** No cross-op transactions —
   same as the redis adapter's per-command atomicity. A
   readdir mid-mutation may skew one scan; the next asked pass
   heals.

This is the registry's own discipline: distribution's
filesystem driver commits blobs via temp-file rename for
exactly the same reason.

## Wiring: deriving the backend

No selector flag — the backend derives from explicit signals
(`os.LookupEnv`, never resolved values, since `KPR_REDIS_ADDR`
carries a default that must not count as a choice):

- `KPR_STORE=file|redis`, when set, is authoritative and must
  agree with backend-specific variables, else boot refuses
  instead of guessing (`file` + `KPR_REDIS_ADDR` conflicts;
  `redis` + `KPR_STORE_DIR` conflicts; unknown values
  refuse).
- Unset: `KPR_STORE_DIR` alone selects file,
  `KPR_REDIS_ADDR` alone selects redis, silence keeps redis
  defaults (current behavior, unchanged).
- `KPR_STORE_DIR` defaults to `kpr` (cwd-relative); compose
  sets it absolute on the shared volume.
- Dev stacks: `docker-compose.yml` is the file backend (default),
  `docker-compose.redis.yml` the redis one. One knob switches
  every make/just target — `make up` vs `COMPOSE=redis make up`
  (same for just). No per-backend targets; `make gc` honors the
  same knob (file: collector under flock in the kpr container;
  redis: collector via `compose run` under the redis lock).

`serve` derives the same way but keeps its lazy semantics
(degrade with a warning, except a backend conflict, which
refuses boot). Refusals name the backend (`storeName`), so
the operator fixes the right thing in either mode.

## The self-contained backup

Point `KPR_STORE_DIR` at `<registry-root>/kpr` and the
registry root carries everything: images, the live
sentinel, kpr rows, locks, run state. Copy/move/snapshot
the root and the whole system — data plus kpr's memory of
it — moves as one unit. The `kpr/` dir is invisible to
the collector, catalog, and upload-purger walks (they
only descend into `docker/registry/v2/repositories`), and
the registry never writes there.

Upgrading backends needs no migration helper today:
backfill rebuilds rows from the registry itself
(`docs/BACKFILL.md`), so a fresh file store refills on
first pass. Deliberately left open otherwise.

## Limits

- **Local volumes only.** flock and fresh mtimes don't
  survive NFS (lockd-dependent, lost on server reboot;
  attribute caching stales mtimes). The NFS downgrade —
  mkdir+heartbeat locks, `noac` mounts — is named but not
  built; NFS here is exotic, a sidequest at most.
- **Single host.** The gc design already assumes the
  local store layout; the file store aligns state with
  that assumption instead of pretending otherwise.
- **Coarse locking.** One dir lock serializes mutating
  ops — fine at kpr's scale, documented for the day it
  isn't.
