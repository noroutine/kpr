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

Three corollaries from the same slices. Promise vs demand
(facades bundle, ports decline): "I am the Store, here is my
interface" (provider's promise — swap implementations behind the
contract) vs "I need Locks from the Store, here is what I need it
to look like" (consumer's demand — only what I use, named for my
intent). The facade lets you depend on *more* while seeing less;
the port lets you depend on *less* while seeing all of it.
Placement tells them apart: interface in `store` = promise,
interface in `gc` = demand. A port that orchestrates is a use case
in costume: kpr's deletion means five things (resolve on
deleted/gone, untrack on held, retry on error, never touch the
unelapsed) — that matrix is the business, and it must live in the
use case. A combined `TagDeleter` either drags the matrix into the
adapter (decisions in externals, untestable without backends) or
reports the outcome back up (reinvented `DeleteManifest`, plus
indirection). Ports abstract externals; they must never contain
decisions. And placement: interfaces sit with their consumers, one
method at a time — `sweep.Registry` (delete),
`keeper.CatalogSource` + `keeper.Prober` (never one two-method
port no consumer fully uses).

Exhibits, all from step 3's autopsy: a `keeper.Marker` port
(Mark/Clear/Unmark are keeper-only) has one package of callers
but a single implementation — and the MemStore tests already prove
more than any marker stub could, so the port would test *less*.
Same for a run-state-read port (`GetCurrent`/`Activity` serve one
use case, one redis behind it). Same for a sweep-side `Locker`
twin (`gc.Locker` already has its consumer; a second twin has
none). Three more ports nobody stands behind — and the clustering
they were meant to document already lives in the per-method docs
("the reap interface", "the plan-discard interface"). A plan dying
to its own rule is the rule working: step 3 closed whole, output
being this paragraph.

— [1b1d33a](https://nrtn.dev/catalyst/kpr/commit/1b1d33ab70d5d22d49770786d572684b5b115e40), [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## W2: A port needs a second implementation that differs in a way someone uses

The rule started life as "never cut a port before its second
user" — and Oleksii's question broke it open twice. First:
`MarkDue` has two keeper callers (`Reap`, `AddPlan`) and still
earned no port — so callers don't count, implementations do.
Second, harder: `Store` *has* two implementations, and so would
every sub-port carved from it, structurally, for free — yet the
split is still ceremony. So the surviving form: a port needs a
second implementation *that differs in a way someone uses* —
different behavior (a stub that fails where the real succeeds),
different trust (a sandboxed caller denied the mark surface),
different backend (marks in a stream, rows in redis). Structural
satisfaction doesn't count; somebody has to want the swap.
`Collector` earned its port (shell stub behind the seam, then the
orchestration consuming it); `Marker` earned nothing (one redis,
MemStore tests already proving more than any stub could). Count
backs of the substitute, never mouths of the caller.

Two exhibits. Deletion is already owned — don't re-own it
cheaply: splitting `Store.Delete` into its own one-method
interface buys a name, not a seam. The tempting version — one port
owning registry-manifest + store-row deletion together — merges
two externals behind one seam and would finally house the orphaned
`Outcome*` vocabulary; either the sweeper's true boundary or a
god-port, and the tiebreaker is the same as ever — a second
implementation asking for it. Until then, sweep-only plus the TTL
floor *is* the boundary, held by convention and contract tests.
Cluster, don't split, a store with one backend: `Store` has
fourteen methods and one redis behind it, so implementation-
splitting is ceremony. What pays is clustering by caller: marks
are keeper-only, run-state reads serve one use case, locks already
collapsed, `Delete` is sweep-only. Narrow by ownership, never by
imagination.

Mirror image for seams: `collectorCommand` (package var, swapped
in tests) is a seam — invisible, global, test-only. `Collector`
is the same need made public: in the signature, usable by any
caller. Ports don't eliminate seams — they push them to the
boundary. A seam used in one test file stays a seam; the second
*consumer* promotes it. Seams are promoted by consumers, ports by
implementations.

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
