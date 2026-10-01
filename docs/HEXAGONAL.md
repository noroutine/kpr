# Hexagonal architecture

kpr is hexagonal: `policy` core, `keeper`/`gc`/`sweep` use
cases, `store`/`registry`/`clock` outbound adapters, `cli`/`web`
driving adapters. It got there one paying slice at a time —
the port map below is the current shape, not the journey.
Dead-simple rule still wins: any new port has to remove a
fake, a duplication, or a scattered wiring block, or it
doesn't get cut.

## Contents

- [Where we are](#where-we-are)
- [Shape](#shape)
- [Standing rules](#standing-rules)
- [Settled questions](#settled-questions)
- [How hexagonal is it](#how-hexagonal-is-it)
- [Focused study: rm --untag and the bypass](#focused-study-rm---untag-and-the-bypass)
- [Future attacks](#future-attacks)

## Where we are

The foundation, still true:

- `policy` is the core. `Row` + `Select*` are pure over
  `(rows, catalogs, now)`. `store` and `sweep` import `policy`,
  so dependencies point inward.
- `store.Store` is a proper outbound port. `RedisStore` +
  `MemStore` + `FileStore` behind it, `storetest/contract.go`
  pins all three to one contract.
- `clock.Source` is the one port the core-adjacent code grew
  after the foundation: `local` (trust the machine, default),
  `https` (a `Date` read), `ntp` (SNTP over UDP) — three
  implementations differing in a way someone uses, which is
  exactly the rule below. Mint timestamps arrive checked;
  future JWT checks reuse the exposed offset. Full approach in
  `docs/TIMESTAMPS.md`.

Closed leaks (receipts in `docs/HEXAGONAL_WISDOMS.md`):

1. `registry` had no port — now `sweep.Registry`,
   `keeper.CatalogSource`, `keeper.Prober`.
2. Use cases lived inside driving adapters — now `keeper`;
   `web` formats, `cli` prints.
3. `Store` is four ports in one — kept fat deliberately (one
   package of callers per cluster, single implementation;
   narrowing would be ceremony).
4. `gc` hid in `cli` — extracted behind
   `Probe`/`Collector`/`Locker`.
5. Wiring scattered across every cobra `RunE` — collapsed
   onto `cli.openDeps`.

Inbound bypass (stays): the mark is the interface. The redis
hash is a public inbound surface that bypasses use cases by
design; the sweeper TTL floor (never wipe before promise
elapses) guards it, and `storetest/contract.go` pins the key
layout.

## Shape

```
policy/                  core (unchanged)
keeper/ gc/             use cases (pure orchestration, ports in)
sweep/                   pass loop (use case where it sits)
store/ registry/ clock/ otel/   outbound adapters
cli/ web/                driving adapters (parse, call, render)
cli.openDeps             the composition root
```

## Standing rules

- One slice at a time, TDD, pipeline green, coverage honest
  (the gate is the unit+e2e union; unit-only gaps need
  naming, not hiding).
- No policy engine, no gRPC/IDL, no framework.
- `keeper` naming: `keeper` (repo vocabulary), not
  `usecase`/`service`.
- New ports stay gated: each needs a "does it pay?" verdict,
  not auto-approval. Measure: lines deleted, fakes deleted,
  duplicated strings gone.

## Settled questions

- `sweep` stays a use case package itself: it keeps the pass
  loop and consumes the `Registry` port, no move under
  `keeper/`.
- Per-consumer store interfaces declined: `Store` stays the
  implementation, clustering documented per-method.
- `gc` ports landed as three tiny interfaces in the `gc`
  package (`Probe`/`Collector`/`Locker`), adapters in `cli`.
- `Clock` correctly dropped — then correctly re-cut as
  `clock.Source` when the second and third transports
  arrived with real behavioral differences (managed-time
  hosts vs sandboxed egress vs precise NTP). A port needs a
  second implementation that differs in a way someone uses;
  time-as-an-argument still covers the pure core.

## How hexagonal is it

Behaviorally, fully: every outbound effect in the use cases goes
through a substitutable port (`sweep.Registry`,
`keeper.CatalogSource`/`Prober`, `gc.Probe`/`Collector`/`Locker`,
`clock.Source`, `store.Store` under a contract all adapters honor),
and unit tests prove it — no HTTP server, no binary, no redis,
no network needed.

Package-graph purists would find three arrows pointing the "wrong"
way: use-case signatures still name adapter packages for types —
`store.Store` + `store.GCLockKey` in `keeper`/`gc`, `registry.Outcome*`
in `sweep`. Those close with a second *differing* implementation
or not at all (a port needs a second implementation
that differs in a way someone uses). Until then, narrowing them
is ceremony with zero substitution payoff.

Deliberate non-textbook bits: the composition root is
`cli.openDeps`, not `cmd/app` injection (restructuring
every cobra var buys nothing); the redis due-mark is a public
inbound surface bypassing use cases by design (guarded by the
sweeper TTL floor).

Accepted review findings, recorded so they stay decided:

- `gc.Run` writes warnings to its `io.Writer` instead of emitting
  events — the one place "core returns data" isn't met. Accepted
  and deferred: warning event types + `renderGCEvent` land with
  the second consumer of gc output, or the next gc touch,
  whichever comes first.
- The three wrong-way type arrows (`store.Store` in `keeper`/`gc`,
  `registry.Outcome*` in `sweep`) stay until a second differing
  implementation asks.
- Sweep-only `Delete` stays convention-held (plus TTL floor and
  contract tests), not interface-held — sense over rulebook at
  this size.

## Focused study: rm --untag and the three misses

`store rm --untag` shipped deleting manifests straight from `cli`.
Review found three misses, one slice each.

### Miss 1 — the bypass

What happened: `cli` cut its own `manifestDeleter`, a
byte-for-byte duplicate of `sweep.Registry`, and called the
registry port directly.

Why it's a problem: the driving adapter reached past the use case
to the outbound port, and the tree grew a second deleter beside
the sweeper — contradicting ARCHITECTURE.md's "sole owner".

What's the fix: no new port (the port rule demands a second
differing implementation, and there is none). A second method on
the same use case (`Sweeper.Untag` beside `RunPass`, the way `gc`
carries `Run`/`Unlock`/`Adopt`): reuse the `Registry` port,
drop-on-confirm, activity records, and the lineage gate; skip the
due-mark requirement, the TTL floor, dry-run, and `Current`
staging. The cli duplicate dies, which pays for the method.
Rejected: mark-due plus trigger-a-pass.

### Miss 2 — no generation

What happened: the delete ties to no proven generation — no mint,
no read-back.

Why it's a problem: nothing distinguishes the registry it
verified from the one it deletes from. A misconfigured
`KPR_REGISTRY_URL` plus a valid digest deletes someone else's
tags with no refusal anywhere.

What's the fix: lineage read-gate inside `Untag`, no mint — an
explicit operator action needs identity, not freshness. Refuse
foreign/unpaired/identity-less/stale outright with the ceremony
named (`rm` carries no `--force`; the remedy is `store adopt`).

### Miss 3 — lock unchecked

What happened: `store lock` denies registry-store writes, but a
manifest DELETE touches no store bytes, so it sails past the lock.

Why it's a problem: undecided whether that's correct. Narrowly
the lock guards mount bytes (and the sweeper's own deletes never
check it either); broadly the lock means "don't mutate my
registry", and a locked operator would be surprised by `--untag`.

What's the fix: decision first, then slice — either extend the
lock to registry deletes, or declare API deletes out of lock
scope in SENTINELS.md and STORES.md. Silent is the only wrong
answer.

Terminology, internalized: `cli`/`web` are driving adapters
(parse, call, render); `keeper`/`gc`/`sweep` are use cases (pure
orchestration over ports); `store`/`registry`/`clock` are outbound
adapters. Adapters never skip the use case to touch a port —
except the one published bypass (redis due-marks), which stays
documented, not repeated.

## Future attacks

Ports are done. The rest of the hexagon, in kpr terms:

- Driving side: only outbound ports cut so far. Shared use cases
  with opposite driver failure policies get a third driver
  when the detached API lands — that's where inbound design stops
  being theory.
- Functional core vs shell: practiced (`policy` pure, time as an
  argument, no `Clock`), never named. The reason the core needs
  no ports at all.
- Events as a boundary: stages, activity ring, gc event stream —
  vocabulary and keys are the contract, wire deferred. Open
  whether emission itself wants a port; today use cases write
  run-state straight to the store.
- Error translation at the boundary: the Outcome matrix and
  gc's refuse-with-remedy style are adapter-error → use-case-
  meaning translation, currently nameless, split across sweep
  and gc.
- Composition root discipline: `openDeps` won by cost, but where
  adapters get built and who owns lifetimes (serve's long-lived
  Run vs one-shot commands) was resolved by feel.
- The designed violation: the redis due-mark bypassing use cases
  is a published language, guarded by the TTL floor. The most
  interesting architectural fact here — understanding why it's
  fine teaches more than any clean port.
