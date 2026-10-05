# kpr documentation

Every page, grouped by what you came here to do. Project overview
and quickstart commands are in the [top-level README](../README.md).

- [Run it](#run-it)
- [Understand it](#understand-it)
- [Hack on it](#hack-on-it)
- [Unbuilt work](#unbuilt-work)

## Run it

| Doc | Answers |
|---|---|
| [QUICKSTART](QUICKSTART.md) | fresh setup: two containers, one volume, first expiring tag |
| [ADOPT](ADOPT.md) | pairing a store to a lineage (`store adopt`) |
| [ADOPT_KPR](ADOPT_KPR.md) | bolting kpr onto your own registry + Traefik |
| [CONFIG](CONFIG.md) | every environment variable, and how they resolve |
| [STORES](STORES.md) | file and redis backends, layouts, invariants |
| [GC](GC.md) | reclaiming blob bytes: the proof chain, the two paths |
| [BACKFILL](BACKFILL.md) | adopting pre-kpr tags into tracked rows |
| [OBSERVABILITY](OBSERVABILITY.md) | Quickwit/Jaeger/Prometheus/Grafana overlay |

## Understand it

| Doc | Answers |
|---|---|
| [ARCHITECTURE](ARCHITECTURE.md) | components, policies, gc, data keys, surfaces |
| [GOALS](GOALS.md) | what kpr is, what it won't become, open questions |
| [PROOFS](PROOFS.md) | the proofs kpr acts on, what each establishes, how they compose |
| [SENTINELS](SENTINELS.md) | same-store proof, locality, lock, lineage verdicts, `kpr store adopt` |
| [EDGE](EDGE.md) | the edge proxy, HOLD/DENY fence, RelativeURLs proof |
| [TIMESTAMPS](TIMESTAMPS.md) | checked clock: transports, wiring, skew semantics |
| [REGISTRY_LAYOUT](REGISTRY_LAYOUT.md) | registry:3 filesystem layout, observed |

## Hack on it

| Doc | Answers |
|---|---|
| [HEXAGONAL](HEXAGONAL.md) | the port map, package bands, dependency rules |
| [HEXAGONAL_WISDOMS](HEXAGONAL_WISDOMS.md) | port-cutting rules learned the hard way |
| [HEXAGONAL_STUDY](HEXAGONAL_STUDY.md) | the post-mortem those rules came from |
| [TESTING](TESTING.md) | unit, e2e, coverage, mutation gates |
| [BUILD](BUILD.md) | builds, releases, cross-compilation |
| [REVIEWER_CONTEXT](REVIEWER_CONTEXT.md) | review loop contract (Claude) |

## Unbuilt work

Designs for work that does not exist yet, kept out of the
reference pages above. Each `X.md` states what ships today; its
`X_FUTURE.md` states what is coming.

| Doc | Answers |
|---|---|
| [ARCHITECTURE_FUTURE](ARCHITECTURE_FUTURE.md) | open and upcoming work |
| [GC_FUTURE](GC_FUTURE.md) | token-auth registries |
| [GC_DANGLING](GC_DANGLING.md) | dangling references: classes, dead-tag-link design |
| [BACKFILL_FUTURE](BACKFILL_FUTURE.md) | shadow reader, digest-less enrichment |
| [PROOFS_FUTURE](PROOFS_FUTURE.md) | proofs over registry config |
| [SENTINELS_FUTURE](SENTINELS_FUTURE.md) | backfill snapshot detection |

Deferred by decision, kept so the reasoning survives:

| Doc | Answers |
|---|---|
| [BLOBCACHE](BLOBCACHE.md) | should kpr speak the descriptor-cache protocol? (not now) |
| [CHILD_REGISTRY](CHILD_REGISTRY.md) | a kpr-launched registry (not started) |
