# Quickstart: kpr on a fresh registry

From zero to expiring tags in ten minutes. No existing stack and no
repo checkout — two containers, one shared volume, no database.

Already run `distribution`? See [ADOPT_KPR.md](ADOPT_KPR.md) instead.

## Contents

- [Prerequisites](#prerequisites)
- [The two files](#the-two-files)
- [Bring it up](#bring-it-up)
- [First expiring tag](#first-expiring-tag)
- [Arming it](#arming-it)
- [Reclaiming blob bytes](#reclaiming-blob-bytes)
- [What to expect](#what-to-expect)

## Prerequisites

Docker with compose, and something to push with — `crane`,
`docker`, or `oras`. The examples use `crane`.

The image is `ghcr.io/noroutine/kpr:<release-tag>`. Pin a tag;
don't float `latest`. No release cut yet? Build one from the repo
and pin `dev`:

```bash
docker build -t ghcr.io/noroutine/kpr:dev .
```

## The two files

**`compose.yml`**

```yaml
services:
  registry:
    image: registry:3
    # Same uid as kpr: the blob store is shared, so both writers
    # must own it or the sentinel mint permission-denies.
    user: "1000:1000"
    volumes:
      - registry-data:/var/lib/registry
      - ./registry-config.yml:/etc/distribution/config.yml:ro
    # No published ports: pushes arrive through the edge below.

  kpr:
    image: ghcr.io/noroutine/kpr:latest   # copy-pasteable; pin a release tag instead, see Prerequisites
    user: "1000:1000"
    volumes:
      - registry-data:/var/lib/registry   # the shared store
      # kpr resolves the store root from the registry's own config,
      # then proves that mount is the store the registry serves.
      - ./registry-config.yml:/etc/distribution/config.yml:ro
    ports:
      # Host pushes land on the edge: a transparent proxy with a
      # HOLD/DENY fence, inside `serve`, default-on.
      - "5000:5000"
      - "9300:9300"   # console; the receiver (:8080) stays internal
    environment:
      - KPR_REGISTRY_URL=http://registry:5000
      # Rows live beside the images, so the registry root is a
      # complete, self-contained backup. Must be absolute.
      - KPR_STORE_DIR=/var/lib/registry/kpr
      # Arm only after the first dry run below:
      # - KPR_CLI_NO_DRY_RUN=true

volumes:
  registry-data:
```

State is plain files by default — no redis, nothing extra to run.

**`registry-config.yml`** — stock `distribution` keys, nothing
kpr-specific server-side:

```yaml
version: 0.1
notifications:
  endpoints:
    - name: kpr
      url: http://kpr:8080/events   # service name, internal network
      timeout: 1s
      threshold: 5
      backoff: 1s
storage:
  delete:
    enabled: true   # without this every sweeper DELETE 405s
  # No `cache:` section on purpose: without `blobdescriptor` every
  # blob stat hits the filesystem, so gc deletions are visible
  # immediately — no stale descriptors, no restart before re-push.
  filesystem:
    rootdirectory: /var/lib/registry
http:
  addr: :5000
  # The edge fronts this registry, so upstream URLs must never name
  # the backend. Never set `host` alongside — it silently overrides
  # this knob, and that fails the edge proof. No proof, no edge.
  relativeurls: true
```

## Bring it up

```bash
docker compose up -d

# Fresh volumes are root-owned; both processes run as uid 1000.
# One-shot claim (the repo's `make up` does this for you):
docker exec -u 0 kpr chown -R 1000:1000 /var/lib/registry

docker exec kpr kpr store unlock    # proves the shared store, pairs it
```

`unlock` mints a baseline sentinel generation and records the
pairing. On a stranger's store it would refuse — here everything is
fresh, so silence means paired.

Console: http://localhost:9300

> **macOS:** AirPlay Receiver squats `localhost:5000` when enabled.
> Disable it, or publish the edge on another port and push there.

## First expiring tag

```bash
crane copy busybox:latest localhost:5000/test/hello:10m
```

Pushes land on the edge and forward byte-identical; the registry
notifies kpr; kpr tracks the row anchored at push time.

```bash
docker exec kpr kpr status    # tracked: 1, due: 0
docker exec kpr kpr plan      # nothing before minute 10
# ...after 10 minutes (or push :10s and wait seconds)...
docker exec kpr kpr reap --no-dry-run   # marks ttl:10m elapsed
docker exec kpr kpr sweep               # dry-run: plans, deletes nothing
```

Still disarmed, so nothing was deleted — the plan shows what
*would* go. `reap` without `--no-dry-run` only prints, `plan add`
marks rows by hand, and `plan discard` clears the plan.

While the store is locked the edge refuses pushes with 423; the
console's Gateway section shows the live posture.

## Arming it

Uncomment `KPR_CLI_NO_DRY_RUN=true`, recreate kpr, and repeat.
This time `sweep` deletes by digest.

## Reclaiming blob bytes

Manifest deletes drop the reference only. `kpr gc` reclaims the
bytes, previewing by default:

```bash
docker exec kpr kpr gc                  # preview, deletes nothing
docker exec kpr kpr gc --no-dry-run     # collect
```

Nothing to restart afterwards: this setup runs without a
blob-descriptor cache, so deletions are visible the moment gc
finishes and a re-push uploads for real. Deployments that *do*
cache descriptors need a restart or a flush first — see
[GC.md](GC.md#after-an-armed-run-stale-blob-descriptors).

## What to expect

- **Tags pushed before kpr arrived are kept.** They have unknown
  age and default to keep. Adopt them with `kpr store backfill`,
  which reads real push times off tag-link mtimes — see
  [BACKFILL.md](BACKFILL.md).
- **Recreating kpr can pause sweeping** for up to the 5-minute
  sweep lock. It self-heals at expiry.
- **A clock-source warning is harmless.** The default `local`
  method checks nothing; with `https` selected, an unreachable
  source warns and proceeds on local time. A genuinely skewed
  clock refuses — fix it and retry. See
  [TIMESTAMPS.md](TIMESTAMPS.md).

Growing past this setup — your own redis, Traefik in front, a
second host? See [ADOPT_KPR.md](ADOPT_KPR.md) and [STORES.md](STORES.md).
