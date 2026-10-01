# Quickstart: kpr on a fresh registry

From zero to expiring tags in ten minutes. No existing stack,
no repo checkout: two containers (stock `registry:3` + kpr),
one shared volume, file state backend — redis not required.
For bolting kpr onto a registry you already run, see
[docs/ADOPT.md](ADOPT.md) instead.

## Contents

- [Prerequisites](#prerequisites)
- [The two files](#the-two-files)
- [Bring it up](#bring-it-up)
- [First expiring tag](#first-expiring-tag)
- [Arming it](#arming-it)
- [Reclaiming blob bytes](#reclaiming-blob-bytes)
- [What to expect](#what-to-expect)

## Prerequisites

Docker with compose, and something to push with (`crane`,
`docker`, `oras` — the example uses `crane`).

The image is `noroutine/kpr:<release-tag>` — pin a tag, don't
float `latest`. (No release cut yet? `docker build -t
noroutine/kpr:dev .` from the repo and pin `dev`.)

## The two files

`compose.yml`:

```yaml
services:
  registry:
    image: registry:3
    # Same uid as kpr (1000): the blob store is shared, so both
    # writers must own it or the sentinel mint permission-denies.
    user: "1000:1000"
    volumes:
      - registry-data:/var/lib/registry
      - ./registry-config.yml:/etc/distribution/config.yml:ro
    ports:
      # Host pushes land here. Needs localhost:5000 free: macOS
      # AirPlay Receiver squats it when enabled — remap or disable.
      - "5000:5000"
  kpr:
    image: noroutine/kpr:<release-tag>   # pin it
    volumes:
      - registry-data:/var/lib/registry   # the shared store
      # gc/unlock resolve the store root from the registry config:
      - ./registry-config.yml:/etc/distribution/config.yml:ro
    ports:
      - "9300:9300"   # console; the receiver (:8080) stays internal
    environment:
      - KPR_STORE=file
      - KPR_STORE_DIR=/var/lib/registry/kpr   # rows live beside images
      - KPR_REGISTRY_URL=http://registry:5000
      # Arm only after the first dry run below:
      # - KPR_SWEEPER_NO_DRY_RUN=true
      # - KPR_TIME_METHOD=https   # checked clock; local default otherwise

volumes:
  registry-data:
```

`registry-config.yml` (stock `distribution` keys — nothing
kpr-specific server-side):

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
  cache:
    blobdescriptor: inmemory   # no redis on this setup
  filesystem:
    rootdirectory: /var/lib/registry
http:
  addr: :5000
```

## Bring it up

```bash
docker compose up -d
# Fresh volumes are root-owned; both processes run as uid 1000.
# One-shot claim (repo `make up` does this for you):
docker exec -u 0 kpr chown -R 1000:1000 /var/lib/registry
docker exec kpr kpr store unlock    # proves the shared store, pairs it
```

`unlock` mints a baseline sentinel generation and records the
pairing. On a stranger's store it refuses — here everything is
fresh, so silence means paired. Console: http://localhost:9300.

## First expiring tag

```bash
crane copy busybox:latest localhost:5000/test/hello:10m
```

The registry notifies kpr; kpr tracks the row anchored at push
time. (If `localhost:5000` is taken, publish the registry's
5000 elsewhere and push there instead.)

```bash
docker exec kpr kpr status    # tracked: 1, due: 0
docker exec kpr kpr plan      # nothing before minute 10
# ...after 10 minutes (or push :10s and wait seconds)...
docker exec kpr kpr reap --no-dry-run   # marks ttl:10m elapsed
docker exec kpr kpr sweep               # dry-run: plans, deletes nothing
```

Still disarmed, nothing deleted: the plan shows what *would*
go. `reap` without `--no-dry-run` only prints; `plan add`
marks rows by hand; `plan discard` clears the plan.

## Arming it

When the plan looks right, uncomment
`KPR_SWEEPER_NO_DRY_RUN=true`, recreate kpr, and repeat —
this time `sweep` deletes by digest and the tick loop picks up
marked rows on its own. Old images pushed before kpr arrived
are kept (unknown age defaults keep — backfill is a known
gap, not silent deletion).

## Reclaiming blob bytes

Manifest deletes drop the reference; blob bytes need the
offline collector: `docker exec kpr kpr gc` previews,
`--no-dry-run` collects after you flip the registry readonly
(config file, restart) — full ceremony in the README's
[Garbage collection](../README.md#garbage-collection) section
and `docs/GC.md`.

## What to expect

- Tags pushed before kpr arrived are kept, never audited.
- Recreating kpr can pause sweeping up to the 5-minute sweep
  lock — self-heals at expiry.
- A clock-source unreachable warning (with `https` selected)
  is harmless: warns, proceeds on local time. A skewed clock
  refuses — fix the clock, retry. Details:
  [docs/TIMESTAMPS.md](TIMESTAMPS.md).
- Growing past this setup (your own redis, Traefik in front,
  a second host): [docs/ADOPT.md](ADOPT.md) and
  [docs/STORES.md](STORES.md).
