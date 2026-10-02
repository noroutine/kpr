# Hexagonal architecture

kpr is hexagonal: `policy` core, `keeper`/`gc`/`sweep` use
cases, `store`/`registry`/`clock` outbound adapters, `cli`/`web`
driving adapters. It got there one paying slice at a time —
the port map below is the current shape, not the journey.
Dead-simple rule still wins: any new port has to remove a
fake, a duplication, or a scattered wiring block, or it
doesn't get cut.

## Contents

- [Hexagonal architecture](#hexagonal-architecture)
  - [Contents](#contents)
  - [Where we are](#where-we-are)
  - [Shape](#shape)
  - [Standing rules](#standing-rules)
  - [Settled questions](#settled-questions)
  - [How hexagonal is it](#how-hexagonal-is-it)
  - [Focused study: rm --untag and the three misses](#focused-study-rm---untag-and-the-three-misses)
    - [Miss 1 — the bypass](#miss-1--the-bypass)
    - [Miss 2 — no generation (open)](#miss-2--no-generation-open)
      - [Evaluation: write surface](#evaluation-write-surface)
      - [Evaluation: enforcing mint and proof](#evaluation-enforcing-mint-and-proof)
      - [Evaluation: producers vs consumers](#evaluation-producers-vs-consumers)
    - [Miss 3 — lock unchecked](#miss-3--lock-unchecked)
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

What happened: `cli` cut its own `manifestDeleter` (duplicate of
`sweep.Registry`) and called the registry port directly.

Problem: driving adapter reaching past the use case — a second
deleter beside the sweeper, against "sole owner".

Fix: no new port. `Sweeper.Untag` / `Sweeper.Untrack` beside
`RunPass`; reuse port, drop-on-confirm, activity; skip due marks,
TTL floor, dry-run, `Current` staging. Duplicate dies.

Landed:
- [ba7e27f](https://nrtn.dev/catalyst/kpr/commit/ba7e27ff71b9bbd67cda39feefeed560b05e9fd2) — `Untag`, cli delegates, duplicate dies.
- [4d06efb](https://nrtn.dev/catalyst/kpr/commit/4d06efb5577f1e7000267787ae38a78c1e664f66) — `Untrack` (bare `rm` delegates, journals `untracked`), partial-output fix, row-drop-failure continuation.

Follow-up (open): `Adopt`'s `pruneSentinelRows` deletes rows
past the sweeper — check whether epoch pruning belongs in
`Sweeper` beside `Untrack`, or stands as the ceremony's own
explicit exception.

Separate outcome — serve/sweep split:

- [a19aa03](https://nrtn.dev/catalyst/kpr/commit/a19aa03896d02a84581236a173ff9537a55cca98) — the sweeper lives in the CLI, `serve` serves endpoints.

What happened: `kpr sweep` puppeteered the pass over HTTP
(POST `/api/sweep` on the console, watch the summary come
back) while `serve` owned the `Sweeper` — the driving adapter
called sideways into another adapter instead of down into the
use case.

Problem: two owners of the pass boundary — the console held
sweep state (`Sweeper` field, armed/dry-run card) it never
decided, and the CLI couldn't run a pass without a server.

Fix: `runSweep` builds the `Sweeper` and calls `RunPass`
in-process (same outcome type, same print); the console drops
the route, the field, and the posture card. Two guardrails
lock it: the console 404s `/api/sweep`, and the degraded
banner asserts no armed/dry-run wording.

### Miss 2 — no generation (open)

What happened: the delete ties to no proven generation.

Problem: a misconfigured `KPR_REGISTRY_URL` plus a valid digest
deletes someone else's tags with no refusal anywhere.

Fix: lineage read-gate inside `Untag`, no mint — evaluation
below. Refuse foreign/unpaired/identity-less/stale outright;
remedy is `store adopt` (`rm` carries no `--force`).

Landed as an arc, not a commit: the fix grew into the sealed
proof package and a structural threading of every delete-adjacent
path. Miss stays open until backfill takes the token too.

- [1cb6531](https://nrtn.dev/catalyst/kpr/commit/1cb65317288890b5fc3ca68e2d6d66b4c5803cf4) — `internal/proof`: seven sealed kinds (`SameStore`, `ArmedRun`, `AcceptedRisk`, `BoundedClock`, `UnlockedStore`, `FreshGeneration`, `RegistryReadonly` / `RegistryWritable`), produced by provers, never crafted; `gc` derives dry-run from the mint.
- [27ca465](https://nrtn.dev/catalyst/kpr/commit/27ca46502cbe5d592714059bf6a1732ef3e2cf40) — mode probe sealed (last gap in the table); presence and dry-run cut as rows (absence, not evidence).
- [398f52a](https://nrtn.dev/catalyst/kpr/commit/398f52a442de5fadf1447f5e4c1264995a99a9f8) — `Untag` takes `SameStore` (nil and stale refuse), `RunPass` shares the `Prover` preamble, `rm --untag` mints at the cobra boundary.
- [5d38b07](https://nrtn.dev/catalyst/kpr/commit/5d38b075d5e861ea886f9f97e45f52415e63f938) — `gc` threaded: `ProveUnlockedStore` opens the run, `Checker` funnels the clock, trailing `AcceptedRisk` gates the writable path plus a demanding collect variant, one shared mint funnel with `unlock`.
- [9d9ea03](https://nrtn.dev/catalyst/kpr/commit/9d9ea03525843be2432d5a4a9473813015bc2ed2) — mutant hunt closed over the arc (one real gap killed, timing equivalents noted).

Remaining: backfill's read gate, `FreshGeneration`'s first consumer, mode tokens in the collect dispatch.

#### Evaluation: write surface

Every write path: the gates each one carries (mint / lineage /
store-lock / clock / mode-probe), who drives it, and what it
drives (signing name in brackets):

| Operation    | Mint?        | Lineage?       | Lock?   | Clock? | Mode? | Driven by          | Drives                                  |
|--------------|--------------|----------------|---------|--------|-------|--------------------|-----------------------------------------|
| `gc` armed   | yes, per run | yes            | yes     | yes    | yes   | human (`gc`)       | mount + rows [`kpr-gc`]                 |
| `unlock`     | yes          | yes            | sets it | yes    | no    | human (`unlock`)   | mount + marker [`kpr-unlock`]           |
| sweeper pass | no           | yes, read gate | no¹     | no     | no    | human (`sweep`)       | registry + rows + activity [`kpr-sweep`] |
| `rm --untag` | no           | yes, token     | no      | no     | no    | human (`rm`)       | registry + rows + activity [`kpr-sweep`] |
| `rm`         | no           | **no**         | no      | no     | no    | human (`rm`)       | rows + activity [`kpr-sweep`]            |
| receiver in  | no           | no             | no      | no     | no    | registry push      | rows [`kpr-receiver`]                   |
| `plan` edits | no           | no             | no      | no     | no    | human (`plan`)     | marks (no signature)                    |

¹ The sweeper takes the single-flight `LockKey`, not the intent
marker — different lock, different meaning.

Readings:

- Minting is concentrated: two call sites, both in `gc`.
  Everything else correctly mints nothing — only gc/unlock
  establish freshness.
- The verdict was evaluated three times by hand: same preamble
  (read served generation → identity + rows → `Judge`), three
  refusal renderings. Extracted into `proof.Prover` — the pass
  consumes the gate, `rm --untag` takes the token (nil and stale
  refuse), callers keep their own refusal rendering.
- Clock and mode-probe are gc-only, correctly so. Deletes work
  either mode (readonly fails visibly); activity timestamps
  aren't proof. No reason to spread either.
- Conclusion: no grand write-gate — operations need genuinely
  different subsets (receiver and plan edits need none). One
  shared lineage preamble, callers render their own refusal.
- Driving in, driven out: humans drive the CLI paths, the
  registry drives intake with push events, humans drive passes
  (`sweep` or POST). What each drives is on the right — and only the
  sweeper serves many errands, so only its activity needs both
  names (actor `kpr-sweep`, trigger `sweep`/POST/`untag`/`rm`).

#### Evaluation: enforcing mint and proof

Miss 2 is a symptom. The problem is enforcement: two invariants
that hold by habit rather than by construction.

- **A — mint on modify.** Every operation that modifies the kpr
  store advances the generation.
- **B — proof on registry.** Every operation that writes the
  registry carries same-store proof.

Where each operation stands against both:

| Operation    | A: mints on modify   | B: proves same store   | Gap                 |
|--------------|----------------------|------------------------|---------------------|
| `gc` armed   | yes, per run         | n/a — no registry write | —                   |
| `unlock`     | yes                  | n/a                    | —                   |
| sweeper pass | no, writes rows      | yes, read gate         | A, if rows count    |
| `rm --untag` | no, writes rows      | **no**                 | both                |
| `rm`         | no, writes rows      | n/a                    | A, if rows count    |
| receiver in  | no, writes rows      | n/a                    | A, if rows count    |
| `plan` edits | no, marks only       | n/a                    | exempt by design    |

Readings:

- Scope A before enforcing it. Taken literally, four operations
  violate it: sweeper, `rm --untag`, `rm` and the receiver all write
  rows and mint nothing. Either the generation tracks mount bytes
  only — rows and activity excluded by definition — or A is far
  larger than Miss 2. The first evaluation's "everything else
  correctly mints nothing" assumes the narrow reading without
  saying so. An invariant with an undefined subject cannot be
  enforced, so this decision comes first.
- Enforcement has tiers, ascending: a doc rule; a shared helper
  callers may call; a test enumerating known paths; a type that
  cannot be constructed outside its prover; an invariant inside the
  port contract. "One shared lineage preamble, callers render their
  own refusal" sits at tier two — the same tier that produced Miss 2,
  since nothing obliges write path eight to call it. Tier three
  still depends on someone remembering to extend the list.
- A belongs in the port contract, not in the use cases. If the
  generation is the store's answer to "which version are you," the
  store advances it, and no mutating method exists that does not.
  That is one assertion in `storetest/contract.go` — after any
  mutation the generation differs — pinned across `RedisStore`,
  `MemStore` and `FileStore` at once. Today the use cases remember
  to mint, which is why minting is concentrated in `gc` by habit
  rather than by construction.
- B belongs in the signature. An opaque proof value with no
  exported constructor, producible only by the prover, sitting in
  the parameter list of every `Sweeper` write. Then write path
  eight cannot be written without first obtaining one — no list to
  maintain, no reviewer to catch it. In Cockburn's template this is
  a precondition used as he means it: not a step performed inside
  the scenario, but the entry condition for being in the scenario.
- No grand write-gate still holds. One gate per invariant, not one
  gate for all five mechanisms. Registry writes are uniform in
  exactly one respect, and that is the one the type carries.
- The enforcement point is the application boundary, so this is the
  first real argument for inbound-port discipline in kpr — not
  "when the detached API lands." If `Sweeper`'s exported writes
  demand a proof, `cli`, `web` and the future API are all
  constrained by construction, and the published redis bypass stays
  the only deliberate exception.

#### Evaluation: producers vs consumers

Agreements first: tiers over helpers (Miss 2 goes to tier four,
not tier two); scope before enforcement; no grand write-gate;
the boundary is where enforcement lives.

One correction: the table above marks B "n/a" for `gc`/`unlock`
— "no registry write". False. The mint *is* a registry write
(blobs to the mount, tag via API). Minting and proving are the
same act there: write, then read back. The minter is the
degenerate prover — it checks nothing, it writes, and the
read-back turns the write into proof. So the roles split clean:

- **Producers** (`gc`, `unlock`): mint freshness. Exempt from
  proof by construction — there is nothing to check yet.
- **Consumers** (sweeper pass, `Untag`, future backfill): take
  proof as input. No proof value, no delete.

And one refusal: A-in-the-port-contract as written. Generations
are registry-side tags; "the store advances it on any mutation"
means a fresh generation per push event (a tag per notification,
keep-N churning at intake frequency) or a second, store-local
version scheme beside sentinel generations. That is a redesign,
not an assertion — parked until someone designs per-mutation
versioning. The scope answer stands as written: the generation
proves mount bytes; rows and activity are versioned by nothing,
and the table should say so instead of hedging per row.

Actionable, in order: `SameStore` sealed plus `Prover.Prove`
cut in `internal/proof` (done); thread it into `RunPass` and
`Untag` (three preambles collapse, callers keep their refusal
rendering); `FreshGeneration` for mint returns
(documentation-grade, mint sites already concentrated);
`Untrack` stays proof-free by explicit decision — row drops
write nothing to the registry.
Receiver and plan paths untouched: no registry writes, nothing
to prove.

### Miss 3 — lock unchecked

What happened: manifest DELETE touches no store bytes, so it sails
past `store lock`.

Problem: undecided if correct — narrowly the lock guards mount
bytes; broadly it means "don't mutate my registry".

Decided: broad — locked means no destructive operations on
registry and store (summarized in `docs/STORES.md`), never
registry-readonly (only registry config enforces that).

Slice: thread `UnlockedStore` into `Sweeper.RunPass` and `Untag`
the way `SameStore` went in — gate at the use case, token in the
signature. `gc` already opens on the marker.

Landed: [b5eb070](https://nrtn.dev/catalyst/kpr/commit/b5eb0709dd7874fb85ae703b6184cbeec8ecee64) — `RunPass` consumes `ProveUnlockedStore` before lineage reads, `Untag`/`Untrack` take the token (unlocked before same); locked refuses in dry-run and armed alike.

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
