# Timestamps: a checked clock, not trusted time

Mint timestamps have to mean something: stale-snapshot detection
compares the served generation against expectations, and a clock
tens of seconds off turns that comparison into false verdicts.
So mints carry checked timestamps — server time compared against
the local clock at mint time — instead of blind ones.

## Contents

- [The port](#the-port)
- [Transports](#transports)
- [Wiring](#wiring)
- [Verdict semantics](#verdict-semantics)
- [Security posture](#security-posture)
- [What was considered and declined](#what-was-considered-and-declined)

## The port

`internal/clock`: one `Source` port, `Offset(ctx, server)`
returning serverTime − localTime at receipt. The offset is
exposed, not just the verdict, so future JWT `iat`/`exp`/`nbf`
checks can reason with it instead of re-deriving it. Tolerance
is a const, `30s`: NTP over the internet lands in milliseconds;
a clock tens of seconds off is broken, not drifting. One
exchange is bounded by a `3s` timeout — a silent server must not
stall a refusal path.

## Transports

Three, one concern per env var (below):

- `local` (default): trusts the machine clock — no external
  check, zero offset, always proceeds. For hosts with managed
  time, and for runs where any network check is noise.
  Checking nothing surprises nobody.
- `https`: a plain `Date` read off a GET. 1s resolution —
  plenty against the 30s tolerance — and it passes wherever
  the web does, which is exactly where sandboxed NTP fails.
  Half-RTT compensated like NTP (symmetric path assumed,
  stated in code): slow links bill half their latency, never
  the full trip. The 1s `Date` truncation drowned a 300ms
  hold in testing, so the hold is 3s with a 10s client.
- `ntp`: stdlib SNTP over UDP. Precise, often egress-blocked
  (the sandbox case that motivated `https`). The
  originate-echo check drops blind off-path replies;
  cancel closes the connection and surfaces `ctx.Canceled`.

## Wiring

- `KPR_TIME_METHOD`: `local` (default), `https`, or `ntp`.
  Unknown values fall back to local with a warning, never a
  refusal.
- `KPR_TIME_SERVER`: the source host for either network
  transport. Default `zeitstempel.dfn.de` — DFN's public time
  service, which answers both NTP and HTTPS, so one default
  serves both. Air-gapped sites point at their own.
- Compose pins `https`: the one place a source is wanted by
  default. Anywhere else, local stays local.

Verified live: `https` mints silently, `ntp` against a
site-local server read +11ms, local default silent,
unreachable source warns, garbage method falls back warned.

## Verdict semantics

The exchange succeeded and the clock is wrong past tolerance
(`SkewError`) → refuse unless overridden. Accepted runs warn;
`unlock` carries no accept flags, so a skewed clock refuses
there outright. The exchange itself failed (unreachable
source) → warn and proceed on local time: an unreachable
server is not evidence of a wrong clock, and air-gapped
sites stay working. Integrity loss stays explicit in the
output either way.

Every altering path opens with this check (`gc`, `unlock`,
the sweeper's gate): clock → lock → mode → lineage → mint.

## Security posture

Neither network transport is authenticated, and neither
needs to be: a spoofed reply only denies (a false skew
refuses the mint). It can neither forge a proof (fs+API
round-trip) nor mint anything. Spoofing is a denial vector
here, not a forgery one — the worst case is a refused run
with the cause printed.

## What was considered and declined

- **Cryptographic timestamps (TSA, RFC 3161):** heavier than
  the threat. The clock only needs "roughly right" (30s) to
  keep generation comparisons honest; a signed timestamp
  proves nothing the fs+API proof doesn't already, and adds
  a CA dependency to every mint path. Declined.
- **A ledger of past offsets:** the offset is measured fresh
  per run and exposed for JWT checks later. No history, no
  store, nothing to reconcile.
