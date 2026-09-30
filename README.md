# kpr (keeper)

Lightweight companion sidecar for a stock OCI `distribution`
registry: ephemeral images and lightweight retention cleanups,
without the weight of Harbor or Nexus. Inspired by ttl.sh.

One binary, one state backend (redis by default, plain files with
`KPR_STORE=file` — no redis required), opinions written as plain
code — no policy engine. Push a tag like `app:10m` and it becomes
eligible for collection 10 minutes after push; `kpr reap` marks it,
the sweeper in `kpr serve` deletes it by digest. Design lives in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md); the current state is in the
[Current state section](docs/ARCHITECTURE.md#current-state) at the end
of that file. State backends (including the redis-less file mode):
[docs/STORES.md](docs/STORES.md).

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
make up            # kpr + redis + registry (detached)
# or: just up

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

Already run `distribution` (behind Traefik or not) and want the keeper
bolted on? Start here: [docs/ADOPT.md](docs/ADOPT.md) — three wires
(notifications, deletes, one redis DB), a registry-config patch, a
copy-paste kpr service with Traefik labels, and a disarmed first run.
Pin `nrtn.dev/catalyst/kpr:<release-tag>`; images publish on tags.

## Policies

Evaluated client-side by `kpr reap`; tunings live next to the code
in `internal/policy`, not in the main config.

| Policy | Reason | Tuning | State |
|---|---|---|---|
| Bare TTL tags (`10m`), eligible after push + TTL | `ttl:10s elapsed` | `DefaultTTL` (off), `MaxTTL` 30d | Done, proven live |
| CI commit builds (`abc1234-10m`): lowercase hex stem of 6+ plus `-ttl` | `ttl:10s elapsed` | `MaxTTL` 30d | Done, proven live |
| Bare hashes (`abc1234`): no suffix, 48h default for next-day triage | `ttl:48h0m0s elapsed` | `HashTTL` 48h | Done, proven live |
| Digest-less rows older than max age (push residue) | `partial:older than 24h` | `StaleUploadMaxAge` 24h | Wired; rarely fires (receiver records digests) |
| Tag vanished from catalog past grace | `untagged:past grace 168h` | `UntaggedGrace` 168h | Wired; needs catalog reads |
| All but N freshest tags per repo | `keep-n:exceeds 10` | `KeepN` 10, **fixed** | Selector tested and runs, but N and include/exclude are not exposed — not a usable policy surface yet (see plan status) |

Hash forms never match (default keep):

- human names with a TTL-shaped tail (`myapp-10m`, `release-7d`)
- all-digit tags (`20240115`, `123456`) — a bare number is a build number
- uppercase hashes (`ABC1234`) — git emits lowercase
- short stems (`a-1h`, `face-7d`)

One honest edge: hex-spellable words of 6+ (`facade-7d`) do match.

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
kpr reap [policy]  # evaluate one policy (expired, partial, untagged,
                   # keep-n) or all; marks accumulate until sweep or
                   # plan discard (--no-dry-run to mark, repeat --exclude
                   # to spare keep-N for matching repo:tag)
kpr sweep    # POST the sweep trigger, print the pass summary
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

`kpr gc` previews by default and refuses a real run rather than
collecting blind: no binary/config mounts, no filesystem store root,
inconclusive sentinel, writable without `--force` (a preview on
writable proceeds warned — it deletes nothing), unproven shared
store, unreachable blobdescriptor cache, another run holding
`kpr:gc:lock` (`make gc` honors the same key). Collection streams the
stock binary's output with stage events (sentinel verdicts, collector
pid, post-probe). After collecting it re-probes: a mode flip mid-run
is loud but never a panic — it fails the run unless `--force`.
It needs the registry's redis password as `REGISTRY_REDIS_PASSWORD`
(same convention the registry uses) — without it the cache
mis-marks and collection eats live layers. Flipping readonly stays
with the operator; the command never rewrites registry config. Manual
collector runs bypass the lock (the registry itself sets none), so
don't run those concurrently either.

## Configuration

Wiring only (ports, redis addr, registry URL, arming); see
[docs/CONFIG.md](docs/CONFIG.md).

| Variable | Meaning |
|---|---|
| `KPR_REDIS_ADDR` | redis (default `localhost:6379`; compose sets `redis:6379`) |
| `KPR_REDIS_PASSWORD` | redis password (empty = no auth; compose sets the shared dev default — `kpr env` shows set/unset only) |
| `KPR_REDIS_DB` | redis logical DB for kpr rows (default `0`; compose sets `4` — DBs 0-2 are taken, 3 is the registry cache) |
| `KPR_STORE` | state backend, `file` or `redis`. Unset means derive: `KPR_STORE_DIR` alone selects file, `KPR_REDIS_ADDR` alone selects redis, silence keeps redis. Must agree with backend vars (see [docs/STORES.md](docs/STORES.md)) |
| `KPR_STORE_DIR` | directory for the file backend (default `kpr/`, cwd-relative; compose sets it absolute on the shared volume, e.g. `<registry-root>/kpr` for a self-contained backup) |
| `KPR_REGISTRY_URL` | registry peer (dev default `http://localhost:5000`) |
| `KPR_SWEEPER_NO_DRY_RUN=true` | arm the serve loop sweeper (anything else keeps implicit dry-run) |
| `KPR_CLI_NO_DRY_RUN=true` | arm one-shot commands (gc collects, reap marks) |

The local compose arms by default (comment the line out to go back
to planning). Recreating the kpr container can pause sweeping for up
to the 5-minute sweep lock if the old instance died mid-pass —
self-heals at lock expiry.

## Observability

Optional overlay, off by default; see
[docs/OBSERVABILITY.md](docs/OBSERVABILITY.md).

```bash
make up-observability   # + Quickwit, Jaeger, Prometheus, Grafana
```

Structured access logs and sweeper activity (`sweep pass` / `sweep
row` records) land in Quickwit; traces in Jaeger; `:9300` links all
four. Testing approach and coverage gates: [docs/TESTING.md](docs/TESTING.md).

## Deliberately out

Policy/workflow engine, scheduler, per-repo rule sets, auth,
signing, replication, cloud integrations, online registry GC. Where
each of these stands is tracked in the
[current state](docs/ARCHITECTURE.md#current-state).

## License

[Your License Here]
