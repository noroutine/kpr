# Hexagonal wisdoms

Lessons from the experiment, each pinned to the commit that taught
it. Read as: the rule, then the receipt. Entries cross-reference
in parentheses ("(Companion: W3)"). W1–W3 and W17 are the
megawisdoms: the first three say what a port is, W17 says when to
cut one — everything else cashes out to it.

## W1 — Megawisdom: ports hide partners, not steps

Two abstractions share one mechanism (the interface), and Go never
labels which you wrote. Procedural abstraction hides *steps*: many
calls → one verb, less visible work (`TagDeleter` bundling the
deletion). A port hides *who*: the capability is exactly as complex
as before — the stub still models all five outcomes — but the use
case no longer knows about HTTP, URLs, or auth. Same complexity,
gone coupling. The trap: reaching for the port mechanism to do a
procedural job compresses steps while smuggling the decisions out
of the core. Procedural abstraction hides steps; ports hide
partners. (Companion: W3, where the wrong kind got proposed.)

## W2 — Megawisdom: facades bundle, ports decline

"I am the Store, here is my interface" (provider's promise — swap
implementations behind the contract) vs "I need Locks from the
Store, here is what I need it to look like" (consumer's demand —
only what I use, named for my intent). The facade lets you depend
on *more* while seeing less: convenient, but every consumer
couples to the whole world behind it. The port lets you depend on
*less* while seeing all of it: the complexity isn't hidden, it's
declined. Same method, two different statements — only the
placement tells them apart (interface in `store` = promise,
interface in `gc` = demand). Meaning stays where the shaping
happens: bundles accumulate decisions, ports leave them in the
core where tests reach them. (See W19 for the store-shaped
version of the same split.)

## W3 — Megawisdom: a port that orchestrates is a use case in costume

Not everything that looks like a port should be one. The test:
follow the *decisions*. kpr's deletion means five things
(resolve on deleted/gone, untrack on held, retry on error, never
touch the unelapsed) — that matrix is the business, and it must
live in the use case. A combined `TagDeleter` port either drags
the matrix into the adapter (decisions in externals, untestable
without backends) or reports the outcome back up (reinvented
`DeleteManifest`, plus indirection). Either way the stub proves
less than the two narrow seams it replaced. Ports abstract
externals; they must never contain decisions. The only honest
version is a Strategy — and with one deletion strategy, there is
nothing to select between. Cut where the second implementation
stands; "orchestrates two steps" is the use case's job
description, not a port's.

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

## W4 — Ports sit with their consumers

The interface belongs next to the code that uses it, one method at
a time — `sweep.Registry` (delete), `cataloger` + `prober` (never
one two-method port no consumer fully uses). Small ports say what
each consumer needs; nothing more.

— [1b1d33a](https://nrtn.dev/catalyst/kpr/commit/1b1d33ab70d5d22d49770786d572684b5b115e40), [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## W5 — Two decouplings, different prices

Decoupling from the *type* (a stub can stand in) buys testability
today. Decoupling from the *package* (the arrow points adapter →
core) pays only with a second implementation. Most day-to-day value
is the first; don't chase the second on principle. (`sweep` still
imports `registry` for `Outcome*`, honestly noted, deliberately
waiting. The second decoupling is W17's currency.)

— [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## W6 — Stubs earn their keep on failure paths

A stub that only replays the happy path proves nothing a fake
server couldn't. The payoff is failures that are awkward over HTTP
and trivial over stubs: a delete erroring mid-pass (row stays due),
a held outcome (row untracked). If your stubs can't fail, your
ports are decoration.

— [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## W7 — Tests must not assume store order

`MemStore.Due` ranges over a map. A test asserting "row A failed,
row B performed" passes either way today — and would read as flaky,
not broken, the day someone adds break-on-failure. Fail the *first
call whatever the ref*, assert against whatever the stub recorded
first. Order-independent, and sensitive to the regression it
claims to pin.

— [d6d65d3](https://nrtn.dev/catalyst/kpr/commit/d6d65d3c498ba0901366a38f3431a161aeaa7f8c)

## W8 — Measures must come out honest

"Kill the HTTP server in sweep tests" meant killing them, not
adding stubs beside them: 12 tests moved, the duplicated held test
deleted, one real-client integration test kept on purpose. Added
coverage that duplicates existing coverage is motion, not progress.

— [d6d65d3](https://nrtn.dev/catalyst/kpr/commit/d6d65d3c498ba0901366a38f3431a161aeaa7f8c)

## W9 — Same shape twice is one port

Two method pairs, two near-identical contract tests, duplicate
bodies in both adapters (`AcquireLock`/`AcquireGCLock`) collapsed
into one named-lock port — especially when the adapter was already
keyed underneath (`redis` had `acquire(ctx, key, ttl)` all along).
Taken because the duplication was exact, not speculative.

— [ac19e97](https://nrtn.dev/catalyst/kpr/commit/ac19e97d7368b08fa72366d6eeb85c3cd9a536ff)

## W10 — A use case with no address shows in its importers

When even the tests reach behavior only through a driving adapter
(e2e importing `cli` for `EvaluatePolicies`), the use case has no
home. Give it a package (`keeper`): behavior now comes from there,
and the only `cli` import left in e2e is `OpenStore` — wiring, not
behavior, and rightly step-5 territory (composition root).

— [88b6ea2](https://nrtn.dev/catalyst/kpr/commit/88b6ea2ccce045376de379c4f88ca2324f4c761e)

## W11 — Adapters parse, call, render — nothing else

`runReap` rendering the plan, `keeperSnapshot` counting outcomes:
both were implementations wearing adapter clothes. After the move,
adapters convert input, call the use case, format output. The
confirmation is in the imports: `cli/keeper.go` no longer names
`policy` at all.

— [d0aa8fd](https://nrtn.dev/catalyst/kpr/commit/d0aa8fd4d9c3a49749c4c15fc7b90f8782129521), [3394e1a](https://nrtn.dev/catalyst/kpr/commit/3394e1a1e1c290fc365278706c70274aadcf1ad9), [d490f2f](https://nrtn.dev/catalyst/kpr/commit/d490f2fcfe942275c84ab0da7a15e190fcd401b1)

## W12 — One counting implementation, two failure policies

`FetchStatus` returns raw data and degrades to red/empty; the CLI
fails on it, the console renders it. Shared logic, opposite
policies — the use case must not choose, or one adapter inherits
the other's error handling. Times stay raw; formatting lives with
the readers.

— [3394e1a](https://nrtn.dev/catalyst/kpr/commit/3394e1a1e1c290fc365278706c70274aadcf1ad9)

## W13 — Behavior tests move with the behavior

Extraction splits tests by kind: behavior (counts, store state,
refusals) moves to the use case's address; message rendering stays
in the adapter. A moved function whose tests stayed behind is a
move you can't prove.

— [d490f2f](https://nrtn.dev/catalyst/kpr/commit/d490f2fcfe942275c84ab0da7a15e190fcd401b1), [88b6ea2](https://nrtn.dev/catalyst/kpr/commit/88b6ea2ccce045376de379c4f88ca2324f4c761e)

## W14 — Moves, not renames — except graduations

Bodies travel untouched; only a port graduating from private seam
to public dependency earns a new name (`cataloger` →
`keeper.CatalogSource`), describing what it provides rather than
who used it. Rename for ownership change, never for style.

— [88b6ea2](https://nrtn.dev/catalyst/kpr/commit/88b6ea2ccce045376de379c4f88ca2324f4c761e)

## W15 — Re-read the receiving file after a move

Diffs don't show floating comments: `Reap` landed between
`EvaluatePolicies`'s doc and its function, and no tool cared.
After moving a function, read the destination top to bottom and
check every doc sits on its own function.

— [61c91f7](https://nrtn.dev/catalyst/kpr/commit/61c91f7e4751066aa5a6ebdb4ecb4fdf7b283b21)

## W16 — Coverage gaps: name them, don't hide them

Extraction-attributable gaps get topped up (direct `ListPlan`
test, zero-branch messages, failing-store fakes). Environmental
gaps (no live redis, helper packages, deferred scope) get named
as out of scope. The gate is the unit+e2e union; unit-only
shortfalls need sentences, not heroics. And wrong doc claims get
corrected in the open ([caa6c8a](https://nrtn.dev/catalyst/kpr/commit/caa6c8afae6f9304404094a4dfe0e2a41ee5c8a5)), not amended into silence.

— [8650e5c](https://nrtn.dev/catalyst/kpr/commit/8650e5c6d77ad8cad721414fb79ee450b816c682), [caa6c8a](https://nrtn.dev/catalyst/kpr/commit/caa6c8afae6f9304404094a4dfe0e2a41ee5c8a5)

## W17 — Megawisdom: second *differing* implementation, not second caller

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
backs of the substitute, never mouths of the caller. (Companion:
W3's exhibits, W18, W19, and W20's mirror image — a seam *is*
promoted by a second consumer, because a seam's whole job is being
used; a port's job is being implemented.)

— [3cf31a8](https://nrtn.dev/catalyst/kpr/commit/3cf31a8e4e0472bd61daae9ae7fbba9bfb4f35c6), [15058c6](https://nrtn.dev/catalyst/kpr/commit/15058c6e8cd2824d517a2302a0fa6444ded3eef4)

## W18 — Deletion is already owned — don't re-own it cheaply

Splitting `Store.Delete` into its own one-method interface buys a
name, not a seam: one method, one consumer, zero new tests. The
tempting version — one port owning registry-manifest + store-row
deletion together — is a bigger claim: it merges two externals
behind one seam and would finally house the orphaned `Outcome*`
vocabulary. That shape is either the sweeper's true boundary or a
god-port; the tiebreaker is the same as ever — a second
implementation asking for it. Until then, sweep-only plus the TTL
floor *is* the boundary, held by convention and contract tests.
(The tiebreaker is W17.)

## W19 — Cluster, don't split, a store with one backend

`Store` has fifteen methods and one redis behind it — no second
implementation is coming, so implementation-splitting is ceremony.
What pays is clustering by caller: marks are keeper-only, run-state
reads serve one use case, locks already collapsed, `Delete` is
sweep-only. Declare ports where a single consumer owns the cluster;
leave `Record`/`All`/`Due`/`Ping` on the shared surface. Narrow by
ownership, never by imagination. (Provider/demand split: W2; the
rule that closed the step: W17.)

## W20 — Seams prototype ports

`collectorCommand` (package var, swapped in tests) is a seam:
invisible, global, test-only. `Collector` is the same need made
public: in the signature, usable by any caller, production passing
the real thing explicitly. Ports don't eliminate seams — they push
them to the boundary (`RunCollector` still bottoms out in the
seam, because something must finally call `exec`). A seam used in
one test file stays a seam; the second consumer promotes it —
note the mirror of W17: seams are promoted by consumers, ports by
implementations.

— [15058c6](https://nrtn.dev/catalyst/kpr/commit/15058c6e8cd2824d517a2302a0fa6444ded3eef4)
