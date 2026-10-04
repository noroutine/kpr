# kpr (keeper)

[![CI](https://github.com/noroutine/kpr/actions/workflows/ci.yml/badge.svg)](https://github.com/noroutine/kpr/actions)
[![codecov](https://codecov.io/gh/noroutine/kpr/branch/master/graph/badge.svg)](https://codecov.io/gh/noroutine/kpr)
[![release](https://img.shields.io/github/v/release/noroutine/kpr)](https://github.com/noroutine/kpr/releases)
[![go](https://img.shields.io/github/go-mod/go-version/noroutine/kpr)](https://go.dev/)
[![license](https://img.shields.io/badge/license-GPLv3-blue)](LICENSE)
[![bolted](https://img.shields.io/badge/bolted_together-noroutine_%26_muse-blue)](https://github.com/noroutine/kpr)

Push `app:10m` to your registry and the tag is gone ten minutes
later.

kpr fronts a stock OCI `distribution` registry: ephemeral images
and lightweight retention cleanup, without running Harbor or Nexus.
Inspired by ttl.sh.

- **One binary.** Goes in front of the registry you already run —
  no fork, no patches, stock `distribution` behind it.
- **One state backend.** Plain files by default — no redis, no
  database, nothing to run. Set `KPR_REDIS_ADDR` to use redis
  instead. See [STORES](docs/STORES.md).
- **Policies are Go code**, not a rule language. Simple, use-case
  driven, tuned by consts.

The cycle is two steps, and nothing runs on its own: `kpr reap`
marks a due tag with a reason, and the sweeper in `kpr serve`
deletes it by digest.

Design and current state live in
[ARCHITECTURE](docs/ARCHITECTURE.md).

## Docs

Full index, grouped by task: **[docs/](docs/README.md)**

Shortcuts to the three most-asked-for pages:

| Doc | Answers |
|---|---|
| [QUICKSTART](docs/QUICKSTART.md) | fresh setup: two containers, one volume, first expiring tag |
| [ADOPT](docs/ADOPT.md) | pairing a store to a lineage (`store adopt`) |
| [ADOPT_KPR](docs/ADOPT_KPR.md) | bolting kpr onto a registry you already run |
| [ARCHITECTURE](docs/ARCHITECTURE.md) | components, policies, gc, data keys, surfaces |

## How it works

1. `docker push` → the registry notifies the receiver in `kpr serve`,
   which records the row (repo/tag/digest/push time) in the state
   backend.
2. `kpr reap` evaluates the policies and **marks** rows due with a
   reason. Dry-run unless `--no-dry-run` — unarmed, it only prints.
3. The sweeper in `kpr serve` (tick backstop, sweep-on-start, or
   `kpr sweep` trigger) **deletes** due rows by digest and resolves
   them: deleted, gone, untracked, planned, failed, or skipped.
4. Console (`:9300`) shows banner, counters, plan, and activity;
   `kpr status` / `kpr plan` show the same as text.

Marking a row due with a reason **is** the interface: anything that
can write the mark (a script, cron, a human with redis-cli) decides
how and when to clean what. Deleting has one owner: the sweeper.

## Quickstart

```bash
make up            # kpr + registry, file backend (detached)
# or: just up        (COMPOSE=redis make up for the redis stack)

# Push something ephemeral (needs localhost:5000 free —
# macOS AirPlay Receiver squats it when enabled):
crane copy busybox:latest localhost:5000/test/busybox:10s

docker exec kpr kpr reap --no-dry-run   # mark (nothing before minute 10)
sleep 15
docker exec kpr kpr reap --no-dry-run   # ttl:10s elapsed
docker exec kpr kpr sweep               # delete by digest, watch the summary
docker exec kpr kpr status              # counters
```

Console: http://localhost:9300. Registry GC (reclaims blob bytes
after manifest deletes — offline, registry stops) is `make gc`.

## Adopting kpr into your own stack

Already run `distribution`, behind Traefik or not? Start here:
[ADOPT_KPR](docs/ADOPT_KPR.md) — two wires (notifications, deletes) plus
the shared volume, a registry-config patch, a copy-paste kpr
service, and a disarmed first run. Pin
`nrtn.dev/catalyst/kpr:<release-tag>`; images publish on tags.

## Reaping policies

`kpr reap [policy]` marks rows due with a reason (`reap all` runs
every policy). Full table: [Reaping policies](docs/ARCHITECTURE.md#reaping-policies).

| Policy | Reason |
| --- | --- |
| `ttl` | `ttl:<d> elapsed` — explicit TTL (`10m`, `myapp-10m`) elapsed since push |
| `hash` | `ttl:<d> elapsed` — bare hash past the default (next-day triage) |
| `partial` | `partial:older than <age>` — digest-less row past the max age |
| `untagged` | `untagged:past grace <grace>` — tracked tag gone from the live catalog |
| `keep-n` | `keep-n:exceeds <n>` — everything past the freshest 10 per repo |

Suffix is intent: any stem + `-ttl` matches (`myapp-10m`, `a-1h`);
bare tags stay hex-scoped (6+ with a letter, `abc1234` yes,
`20240115` no). Out: dotted versions (`v1.2.3-1h`), dangling forms
(`abc1234-`, `10m-`), uppercase units (`10M`).

Spared: `latest` is never swept by any policy and never counts into
keep-N — a repo keeps 10 plus `latest`.

The sweeper adds its own floor regardless of marks: a TTL row whose
promise hasn't elapsed is never wiped by a stale mark.

## CLI

```bash
kpr serve    # console :9300 + app :8080 + receiver + sweeper loop
kpr status   # banner + counters as text
kpr plan     # pending candidates (--json for piping)
kpr plan discard  # drop the whole plan (clear due marks, no dry-run)
kpr plan add <pattern>...     # mark tracked repo:tag by glob, regex: or
                             # exact image (exact names must match; no dry-run)
kpr plan remove <pattern>...  # unmark due rows by glob, regex: or exact image (no dry-run)
kpr store ls  # tracked rows, short columns (--long, --json; ls sentinels)
kpr store inspect <repo:tag>  # one full row (exact spelling)
kpr store rm <repo:tag>...  # drop rows; tag stays, untracked (no dry-run)
kpr store rm --untag <repo:tag>...  # delete the manifest too, row drops on confirm
kpr store unlock   # prove the shared store, set the intent marker
kpr store lock     # drop the intent marker
kpr store adopt [IDENT] [--gen]  # pair the store to the served lineage
kpr store status  # backend, lock, proof, identity, activity tail (--json)
kpr reap [policy]  # evaluate one policy (ttl, hash, partial,
                   # untagged, keep-n) or all; marks accumulate until
                   # sweep or plan discard (--no-dry-run to mark, repeat
                   # --exclude to spare keep-N for matching repo:tag)
kpr registry analyze  # six live lines, catalog/store/fs first
                      # (needs a filesystem KPR_REGISTRY_CONFIG),
                      # then revisions, blobs, bytes (--json exact)
kpr registry ls sentinels  # machinery tags as the registry sees
                      # them, with evaluated identity (--json, --long)
kpr sweep    # run one sweep pass in-process, print the summary
kpr gc       # garbage-collect the shared store (dry-run preview by
             # default; --no-dry-run collects, readonly probe first)
kpr env      # resolved configuration
```

The keeper CLI talks to state directly, so it runs colocated with
`serve` (same network for redis, same volume for files:
`docker exec kpr kpr …`). Detached operation
is explicitly deferred.

## Garbage collection

Deletes drop the manifest reference only; blob bytes need the stock
collector against the shared store:

```bash
# 0. Preview anytime (dry-run default, streams the collector):
docker exec kpr kpr gc
# 1. Registry readonly (config file! the env override panics
#    registry:3): storage.maintenance.readonly.enabled: true + restart.
# 2. Collect from the kpr container (shared mounts, same binary):
docker exec kpr kpr gc --no-dry-run                 # refuses unless readonly
docker exec kpr kpr gc --no-dry-run --delete-untagged
# 3. Flip readonly back off + restart.
```

`kpr gc` previews by default. An armed run refuses rather than
collect blind, when:

- the binary or config mounts are missing
- the store root is not a local filesystem
- the sentinel verdict is inconclusive
- the registry is serving without online clearance
- the shared store is unproven
- the blobdescriptor cache is unreachable (offline path)
- another run holds `kpr:gc:lock` (`make gc` honors the same key)

A preview prints the same checklist, warns, and proceeds — it
deletes nothing either way.

Two things to know before arming:

- **Set `REGISTRY_REDIS_PASSWORD`** (same convention the registry
  uses). Without it the cache mis-marks and collection deletes
  live layers.
- **Don't run the stock collector alongside.** Manual runs take no
  lock, because the registry sets none.

During a run the collector's output streams through with stage
events: sentinel verdicts, collector pid, post-probe. Afterwards gc
re-probes the mode. A flip mid-run fails the run unless
`--accept-mode-flip` — loud, never a panic.

Flipping readonly stays with the operator. The command never
rewrites registry config.

## Configuration

Wiring only (ports, redis addr, registry URL, arming); see
[docs/CONFIG.md](docs/CONFIG.md).

| Variable | Meaning |
|---|---|
| `KPR_REDIS_ADDR` | redis (default `localhost:6379`; compose sets `redis:6379`) |
| `KPR_REDIS_PASSWORD` | redis password (empty = no auth; compose sets the shared dev default — `kpr env` shows set/unset only) |
| `KPR_REDIS_DB` | redis logical DB for kpr rows (default `0`; compose sets `4` — DBs 0-2 are taken, 3 is the registry cache) |
| `KPR_STORE` | state backend, `file` (default) or `redis`. Unset means derive: `KPR_STORE_DIR` alone selects file, `KPR_REDIS_ADDR` alone selects redis, silence selects file. Must agree with backend vars (see [STORES](docs/STORES.md)) |
| `KPR_STORE_DIR` | directory for the file backend (default `kpr/`, cwd-relative; compose sets it absolute on the shared volume, e.g. `<registry-root>/kpr` for a self-contained backup) |
| `KPR_REGISTRY_URL` | registry peer (dev default `http://localhost:5000`) |
| `KPR_EDGE_ADDR` | edge proxy listen address inside serve (default `:5000` — the registry's published port, moved to the edge) |
| `KPR_EDGE=false` | run serve without the edge proxy (default-on; a failed RelativeURLs proof also closes it loudly — see [docs/GATEWAY.md](docs/GATEWAY.md)) |
| `KPR_CLI_NO_DRY_RUN=true` | arm one-shot commands (gc collects, reap marks, sweep deletes) |
| `KPR_TIME_METHOD` | checked-clock transport: `local` (default), `https`, `ntp` (see [docs/TIMESTAMPS.md](docs/TIMESTAMPS.md); compose pins `https`) |
| `KPR_TIME_SERVER` | time source host (default `zeitstempel.dfn.de`; air-gapped sites point at their own) |

The local compose arms by default (comment the line out to go back
to planning). Recreating the kpr container can pause sweeping for up
to the 5-minute sweep lock if the old instance died mid-pass —
self-heals at lock expiry.

## Observability

Optional overlay, off by default; see
[docs/OBSERVABILITY.md](docs/OBSERVABILITY.md).

```bash
make up   # + Quickwit, Jaeger, Prometheus, Grafana (up-minimal for the base stack)
```

Structured access logs and sweeper activity (`sweep pass` / `sweep
row` records) land in Quickwit; traces in Jaeger; `:9300` links all
four. Testing approach and coverage gates: [docs/TESTING.md](docs/TESTING.md).

## Deliberately out

Policy/workflow engine, scheduler, per-repo rule sets, auth,
signing, replication, cloud integrations, online registry GC. The
reasoning for each is in
[ARCHITECTURE](docs/ARCHITECTURE.md#deliberately-out).

## License

GPLv3 ([LICENSE](LICENSE)). Third-party attributions ship in
[LICENSES/](LICENSES/) and inside the image at `/app/licenses/`.

