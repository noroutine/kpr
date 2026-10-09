# Hexagonal wisdoms

Lessons from building kpr hexagonal, each pinned to the commit that taught
it. The rule, then the receipt. Two megawisdoms up top, then the checklist.

## Contents

- [W1: Ports hide partners, not steps](#w1-ports-hide-partners-not-steps)
- [W2: A port needs a second implementation that differs in a way someone uses](#w2-a-port-needs-a-second-implementation-that-differs-in-a-way-someone-uses)
- [W3: Two decouplings, different prices](#w3-two-decouplings-different-prices)
- [W4: Stubs earn their keep on failure paths](#w4-stubs-earn-their-keep-on-failure-paths)
- [W5: Tests must not assume store order](#w5-tests-must-not-assume-store-order)
- [W6: Measures must come out honest](#w6-measures-must-come-out-honest)
- [W7: Same shape twice is one port](#w7-same-shape-twice-is-one-port)
- [W8: One counting implementation, two failure policies](#w8-one-counting-implementation-two-failure-policies)
- [W9: Behavior tests move with the behavior](#w9-behavior-tests-move-with-the-behavior)
- [W10: Moves, not renames — except graduations](#w10-moves-not-renames--except-graduations)
- [W11: Re-read the receiving file after a move](#w11-re-read-the-receiving-file-after-a-move)
- [W12: Coverage gaps: name them, don't hide them](#w12-coverage-gaps-name-them-dont-hide-them)
- [W13: Seams are proto-ports](#w13-seams-are-proto-ports)
- [W14: A declined port can earn its way back](#w14-a-declined-port-can-earn-its-way-back)
- [W15: Core takes readings, never readers](#w15-core-takes-readings-never-readers)
- [W16: One field per splittable role](#w16-one-field-per-splittable-role)
- [W17: Narration is local, history is shared](#w17-narration-is-local-history-is-shared)

## W1: Ports hide partners, not steps

Two abstractions share one mechanism (the interface), and Go never
labels which you wrote. Procedural abstraction hides *steps*: many
calls → one verb, less visible work (`TagDeleter` bundling the
deletion). A port hides *who*: the capability is exactly as complex
as before — the stub still models all five outcomes — but the use
case no longer knows about HTTP, URLs, or auth. Same complexity,
gone coupling. The trap: reaching for the port mechanism to do a
procedural job compresses steps while smuggling the decisions out
of the core.

Corollaries, kept because we discovered them. Promise vs
demand: "I am the Store, here is my interface" (provider's
promise) vs "I need Locks from the Store, here is what I need"
(consumer's demand). The facade lets you depend on *more* while
seeing less; the port on *less* while seeing all of it —
interface in `store` = promise, in `gc` = demand. A port that
orchestrates is a use case in costume: kpr's deletion means five
things (resolve on deleted/gone, untrack on held, retry on error,
never touch the unelapsed), and that matrix must live in the use
case. A combined `TagDeleter` either drags it into the adapter or
reports the outcome back up (reinvented `DeleteManifest`, plus
indirection). Ports abstract externals; they never contain
decisions. Placement: interfaces sit with their consumers, one
method at a time — `sweep.Registry` (delete),
`keeper.CatalogSource` + `keeper.Prober` (never one two-method
port no consumer fully uses).

Rejected ports, all from step 3: a `keeper.Marker` port (one
package of callers, one implementation — MemStore tests already
prove more than any marker stub could), a run-state-read port
(one use case, one redis), a sweep-side `Locker` twin
(`gc.Locker` already has its consumer; a second twin has none).
The clustering they were meant to document already lives in the
per-method docs.

— [1b1d33a](https://nrtn.dev/catalyst/kpr/commit/1b1d33ab70d5d22d49770786d572684b5b115e40), [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## W2: A port needs a second implementation that differs in a way someone uses

Callers don't count, implementations do: `MarkDue` has two
keeper callers (`Reap`, `AddPlan`) and still earned no port. And
structural satisfaction doesn't count either: `Store` *has* two
implementations, and so would every sub-port carved from it, for
free — yet the split is still ceremony. The rule: a port needs a
second implementation *that differs in a way someone uses* —
different behavior (a stub that fails where the real succeeds),
different trust (a sandboxed caller denied the mark surface),
different backend (marks in a stream, rows in redis). Somebody has
to want the swap. `Collector` earned its port (shell stub behind
the seam, then the orchestration consuming it); `Marker` earned
nothing (one redis, MemStore tests already proving more than any
stub could).

Two exhibits. Deletion stays where it is: splitting
`Store.Delete` into its own one-method interface buys a name, not
a seam, and the tempting combined registry+store deletion port
merges two externals behind one seam — the sweeper's true
boundary or a god-port, decided by whether a second
implementation asks for it. Until then, sweep-only plus the TTL
floor *is* the boundary, held by convention and contract tests.
Cluster, don't split, a store with one backend: `Store` has
fourteen methods and one redis behind it. What pays is clustering
by caller — marks are keeper-only, run-state reads serve one use
case, locks already collapsed, `Delete` is sweep-only. Narrow by
ownership, never by imagination. (Seam side of this rule: W13.)

— [3cf31a8](https://nrtn.dev/catalyst/kpr/commit/3cf31a8e4e0472bd61daae9ae7fbba9bfb4f35c6), [15058c6](https://nrtn.dev/catalyst/kpr/commit/15058c6e8cd2824d517a2302a0fa6444ded3eef4)

## W3: Two decouplings, different prices

Decoupling from the *type* (a stub can stand in) buys testability
today. Decoupling from the *package* (the arrow points adapter →
core) pays only with a second implementation. Most day-to-day value
is the first; don't chase the second on principle. (`sweep` still
imports `registry` for `Outcome*`, honestly noted, deliberately
waiting. The second decoupling is W2's currency.)

— [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## W4: Stubs earn their keep on failure paths

A stub that only replays the happy path proves nothing a fake
server couldn't. The payoff is failures that are awkward over HTTP
and trivial over stubs: a delete erroring mid-pass (row stays due),
a held outcome (row untracked). If your stubs can't fail, your
ports are decoration.

— [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## W5: Tests must not assume store order

`MemStore.Due` ranges over a map. A test asserting "row A failed,
row B performed" passes either way today — and would read as flaky,
not broken, the day someone adds break-on-failure. Fail the *first
call whatever the ref*, assert against whatever the stub recorded
first. Order-independent, and sensitive to the regression it
claims to pin.

— [d6d65d3](https://nrtn.dev/catalyst/kpr/commit/d6d65d3c498ba0901366a38f3431a161aeaa7f8c)

## W6: Measures must come out honest

"Kill the HTTP server in sweep tests" meant killing them, not
adding stubs beside them: 12 tests moved, the duplicated held test
deleted, one real-client integration test kept on purpose. Added
coverage that duplicates existing coverage is motion, not progress.

— [d6d65d3](https://nrtn.dev/catalyst/kpr/commit/d6d65d3c498ba0901366a38f3431a161aeaa7f8c)

## W7: Same shape twice is one port

Two method pairs, two near-identical contract tests, duplicate
bodies in both adapters (`AcquireLock`/`AcquireGCLock`) collapsed
into one named-lock port — especially when the adapter was already
keyed underneath (`redis` had `acquire(ctx, key, ttl)` all along).
Taken because the duplication was exact, not speculative.

— [ac19e97](https://nrtn.dev/catalyst/kpr/commit/ac19e97d7368b08fa72366d6eeb85c3cd9a536ff)

## W8: One counting implementation, two failure policies

`FetchStatus` returns raw data and degrades to red/empty; the CLI
fails on it, the console renders it. Shared logic, opposite
policies — the use case must not choose, or one adapter inherits
the other's error handling. Times stay raw; formatting lives with
the readers.

— [3394e1a](https://nrtn.dev/catalyst/kpr/commit/3394e1a1e1c290fc365278706c70274aadcf1ad9)

## W9: Behavior tests move with the behavior

Extraction splits tests by kind: behavior (counts, store state,
refusals) moves to the use case's address; message rendering stays
in the adapter. A moved function whose tests stayed behind is a
move you can't prove.

— [d490f2f](https://nrtn.dev/catalyst/kpr/commit/d490f2fcfe942275c84ab0da7a15e190fcd401b1), [88b6ea2](https://nrtn.dev/catalyst/kpr/commit/88b6ea2ccce045376de379c4f88ca2324f4c761e)

## W10: Moves, not renames — except graduations

Bodies travel untouched; only a port graduating from private seam
to public dependency earns a new name (`cataloger` →
`keeper.CatalogSource`), describing what it provides rather than
who used it. Rename for ownership change, never for style.

— [88b6ea2](https://nrtn.dev/catalyst/kpr/commit/88b6ea2ccce045376de379c4f88ca2324f4c761e)

## W11: Re-read the receiving file after a move

Diffs don't show floating comments: `Reap` landed between
`EvaluatePolicies`'s doc and its function, and no tool cared.
After moving a function, read the destination top to bottom and
check every doc sits on its own function.

— [61c91f7](https://nrtn.dev/catalyst/kpr/commit/61c91f7e4751066aa5a6ebdb4ecb4fdf7b283b21)

## W12: Coverage gaps: name them, don't hide them

Extraction-attributable gaps get topped up (direct `ListPlan`
test, zero-branch messages, failing-store fakes). Environmental
gaps (no live redis, helper packages, deferred scope) get named
as out of scope. The gate is the unit+e2e union; unit-only
shortfalls need sentences, not heroics. And wrong doc claims get
corrected in the open ([caa6c8a](https://nrtn.dev/catalyst/kpr/commit/caa6c8afae6f9304404094a4dfe0e2a41ee5c8a5)), not amended into silence.

— [8650e5c](https://nrtn.dev/catalyst/kpr/commit/8650e5c6d77ad8cad721414fb79ee450b816c682), [caa6c8a](https://nrtn.dev/catalyst/kpr/commit/caa6c8afae6f9304404094a4dfe0e2a41ee5c8a5)

## W13: Seams are proto-ports

`collectorCommand` (package var, swapped in tests) is a seam:
invisible, global, test-only. `Collector` is the same need grown
up: in the signature, usable by any caller, production passing the
real thing explicitly. Start with the seam — it's one line and it
proves the substitution matters. Promote to a port when the second
consumer arrives (gc-4's orchestration consuming it); a seam used
in one test file stays a seam. Ports don't eliminate seams, they
push them to the boundary: `runCollector` still bottoms out in the
seam, because something must finally call `exec`.

Demotion (the static-vs-computed rule): `Probe`/`Collect` went back
from `Deps` fields to package vars. Their production values are
constant — `probeRegistry`/`runCollector` on every run, no inputs —
so the fields carried no per-run variation, only test substitution,
which a seam does in one line. `Report`/`Fence`/`Clock` stayed
ports: each computes from the run (writer, store+armed, config).
Ports carry what varies; seams carry what merely substitutes.

— [7ac2020](https://nrtn.dev/catalyst/kpr/commit/7ac2020ec4c25cde304b0abd23064cbd146c862d), [15058c6](https://nrtn.dev/catalyst/kpr/commit/15058c6e8cd2824d517a2302a0fa6444ded3eef4)

## W14: A declined port can earn its way back

`Clock` was correctly dropped — time-as-an-argument covered
the pure core, and one implementation is a seam, not a port.
Then three transports arrived differing in a way someone
uses: managed-time hosts wanting no check (`local`), sandboxed
egress where UDP never leaves (`https`), precise sync where
it does (`ntp`). Same rule as W2, run in reverse: the second
*differing* implementation is the event, not the calendar.
A "never" in the standing rules is a verdict on the evidence
at the time; new evidence re-opens exactly that verdict, not
every neighboring one (the core still takes time as an
argument — only the mint path grew the port).

## W15: Core takes readings, never readers

The temptation: `proof.Arm(cmd, cfg)` — one call that reads the
flag and the env itself, so every command "gets arming
automatically". Declined: the core would import cobra and
config, and the adapter arrow would point inward. Evidence
flows inward means *values* flow inward — the adapter reads its
own sources and passes bools (`proof.Arm(gcNoDryRun,
cfg.CLINoDryRun)`). Two prices killed the shortcut: a stringly
flag lookup fails at runtime where a bound var fails at compile
time, and per-command sources differ anyway
(each command's own flag var over the shared `CLINoDryRun`),
so the "automatic" call needs parameters for which sources —
saving nothing over two bools. The automatic part comes from the other side: stages
take `ArmedRun`, so a command that forgets the one line doesn't
compile.

## W16: One field per splittable role

`gcrun.Deps` carries one store four times — `Lock`, `Rec`,
`Ids`, `Rows` — and production passes the same object into all
four, so the temptation is a single `Store` field. Resisted: the
tests script each role apart (unreadable lock marker, failing
keep-N log, unreadable/unrecordable lineage, untracked rows),
and one field would force every fake to implement the whole
store just to break one role. One object N times is not
duplication — it is `wirePorts` saying the four hats sit on one
head. Collapse fields only when no test tells the roles apart.

— [656b1a4](https://nrtn.dev/catalyst/kpr/commit/656b1a447dd26ed2ab19f110849cf6c16c58111f)

## W17: Narration is local, history is shared

The fence adapter voiced transitions into the use case's event
stream, coupling `edge` to `gc` for lines nobody rendered:
Deny/Allow pass nil, Hold's stages fall through the renderer.
The shared sink was always the ring — state can't time-travel,
so transitions must be recorded where both worlds read. Each
process now voices its own output (the run its hold lines, the
Gate serve's log) and shares only history. An announce path
with no rendering consumer is not a channel, it's a habit:
prove the consumer before keeping the type.

— [d7f9f6d](https://nrtn.dev/catalyst/kpr/commit/d7f9f6df047bdf632f9ffd85d63ca49bb39ea92e)
