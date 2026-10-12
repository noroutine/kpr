# Receiver <!-- omit in toc -->

The registry receiver: `POST /events` on its own port (`:9000` by
default). Push notifications become tracked rows, delete
notifications drop them. Nothing else lives here.

## Contents <!-- omit in toc -->
- [Why a separate port](#why-a-separate-port)
- [Contract](#contract)
- [Configuration](#configuration)
- [Topology](#topology)
- [Counters](#counters)

## Why a separate port

Three audiences, three ports. The app landing page and whatever APIs
materialize there are user-facing (`:8080`). The console is
management-facing (`:9300`). `/events` is neither: it is registry
coupling — a machine webhook the registry fires on every push and
delete.

Bundling `/events` with either of the other two creates
configuration dilemmas: both may sit in networks where leaving a
registry webhook open is undesirable, yet both need publishing for
their own audiences. On its own port the topologies turn flexible
at the cost of one extra port: publish `:9000` where the registry
reaches it, nowhere else.

## Contract

One route, one method shape:

- `POST /events` with a distribution notification envelope →
  `202 Accepted`. Pushes record rows signed `kpr-receiver`, deletes
  drop them (see [ARCHITECTURE](ARCHITECTURE.md) for the data
  vocabulary).
- Unparseable envelope → `400`, never a panic and never silent.
- Nil or unreachable store → `503` on the envelope that needs it,
  `202` with a logged drop on the write path: a registry retry
  storm during an outage is worse than a visible gap.
- Anything else → `404`. A miswired registry or a curious browser
  learns nothing here.

The row semantics (stale-delete guard, push-time anchor, digestless
delete) are unchanged by the move — only the listener changed.
Handler behavior is pinned in `internal/receiver/events_test.go`.

## Configuration

- `KPR_RECEIVER_HOST` (default `::`) and `KPR_RECEIVER_PORT`
  (default `9000`); flags `--receiver-host` / `--receiver-port`
  override per invocation, like the other servers.
- Invalid ports fail soft to `9000` with a startup warning, like
  the other servers.
- The registry webhook URL follows the port:
  `http://kpr:9000/events` (service name on the shared network).

## Topology

The registry only needs the service port: `http://kpr:9000/events`
resolves container-to-container with no host publish. The compose
stacks keep `9000:9000` commented out for exactly this reason —
uncomment it only when an operator (or a debugging `curl`) needs
host-side access. The quickstart and adopt snippets never publish
it at all.

Traefik never fronts the receiver: notifications from inside the
registry container to a public URL would hairpin through TLS and
any auth middleware. The receiver stays off it, like before —
only its port number changed.

## Counters

The console's Requests metric counts what comes via the app port
(landing page, `/api/status`). Receiver traffic is not in that
number. Edge proxied requests and event notifications each want
their own counters eventually; that mediation is still open, and
this split is what makes it possible — per-port counters now have
a port each to hang on.
