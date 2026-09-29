# Hexagonal wisdoms

Lessons from the experiment, each pinned to the commit that taught
it. Read as: the rule, then the receipt.

## Ports sit with their consumers

The interface belongs next to the code that uses it, one method at
a time — `sweep.Registry` (delete), `cataloger` + `prober` (never
one two-method port no consumer fully uses). Small ports say what
each consumer needs; nothing more.

— [1b1d33a](https://nrtn.dev/catalyst/kpr/commit/1b1d33ab70d5d22d49770786d572684b5b115e40), [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## Two decouplings, different prices

Decoupling from the *type* (a stub can stand in) buys testability
today. Decoupling from the *package* (the arrow points adapter →
core) pays only with a second implementation. Most day-to-day value
is the first; don't chase the second on principle. (`sweep` still
imports `registry` for `Outcome*`, honestly noted, deliberately
waiting.)

— [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## Stubs earn their keep on failure paths

A stub that only replays the happy path proves nothing a fake
server couldn't. The payoff is failures that are awkward over HTTP
and trivial over stubs: a delete erroring mid-pass (row stays due),
a held outcome (row untracked). If your stubs can't fail, your
ports are decoration.

— [c8cdf03](https://nrtn.dev/catalyst/kpr/commit/c8cdf03073cb06301aaa676e08b07fcf7629c66f)

## Tests must not assume store order

`MemStore.Due` ranges over a map. A test asserting "row A failed,
row B performed" passes either way today — and would read as flaky,
not broken, the day someone adds break-on-failure. Fail the *first
call whatever the ref*, assert against whatever the stub recorded
first. Order-independent, and sensitive to the regression it
claims to pin.

— [d6d65d3](https://nrtn.dev/catalyst/kpr/commit/d6d65d3c498ba0901366a38f3431a161aeaa7f8c)

## Measures must come out honest

"Kill the HTTP server in sweep tests" meant killing them, not
adding stubs beside them: 12 tests moved, the duplicated held test
deleted, one real-client integration test kept on purpose. Added
coverage that duplicates existing coverage is motion, not progress.

— [d6d65d3](https://nrtn.dev/catalyst/kpr/commit/d6d65d3c498ba0901366a38f3431a161aeaa7f8c)

## Same shape twice is one port

Two method pairs, two near-identical contract tests, duplicate
bodies in both adapters (`AcquireLock`/`AcquireGCLock`) collapsed
into one named-lock port — especially when the adapter was already
keyed underneath (`redis` had `acquire(ctx, key, ttl)` all along).
Taken because the duplication was exact, not speculative.

— [ac19e97](https://nrtn.dev/catalyst/kpr/commit/ac19e97d7368b08fa72366d6eeb85c3cd9a536ff)

## A use case with no address shows in its importers

When even the tests reach behavior only through a driving adapter
(e2e importing `cli` for `EvaluatePolicies`), the use case has no
home. Give it a package (`keeper`): behavior now comes from there,
and the only `cli` import left in e2e is `OpenStore` — wiring, not
behavior, and rightly step-5 territory (composition root).

— [88b6ea2](https://nrtn.dev/catalyst/kpr/commit/88b6ea2ccce045376de379c4f88ca2324f4c761e)

## Adapters parse, call, render — nothing else

`runReap` rendering the plan, `keeperSnapshot` counting outcomes:
both were implementations wearing adapter clothes. After the move,
adapters convert input, call the use case, format output. The
confirmation is in the imports: `cli/keeper.go` no longer names
`policy` at all.

— [d0aa8fd](https://nrtn.dev/catalyst/kpr/commit/d0aa8fd4d9c3a49749c4c15fc7b90f8782129521), [3394e1a](https://nrtn.dev/catalyst/kpr/commit/3394e1a1e1c290fc365278706c70274aadcf1ad9), [d490f2f](https://nrtn.dev/catalyst/kpr/commit/d490f2fcfe942275c84ab0da7a15e190fcd401b1)

## One counting implementation, two failure policies

`FetchStatus` returns raw data and degrades to red/empty; the CLI
fails on it, the console renders it. Shared logic, opposite
policies — the use case must not choose, or one adapter inherits
the other's error handling. Times stay raw; formatting lives with
the readers.

— [3394e1a](https://nrtn.dev/catalyst/kpr/commit/3394e1a1e1c290fc365278706c70274aadcf1ad9)

## Behavior tests move with the behavior

Extraction splits tests by kind: behavior (counts, store state,
refusals) moves to the use case's address; message rendering stays
in the adapter. A moved function whose tests stayed behind is a
move you can't prove.

— [d490f2f](https://nrtn.dev/catalyst/kpr/commit/d490f2fcfe942275c84ab0da7a15e190fcd401b1), [88b6ea2](https://nrtn.dev/catalyst/kpr/commit/88b6ea2ccce045376de379c4f88ca2324f4c761e)

## Moves, not renames — except graduations

Bodies travel untouched; only a port graduating from private seam
to public dependency earns a new name (`cataloger` →
`keeper.CatalogSource`), describing what it provides rather than
who used it. Rename for ownership change, never for style.

— [88b6ea2](https://nrtn.dev/catalyst/kpr/commit/88b6ea2ccce045376de379c4f88ca2324f4c761e)

## Re-read the receiving file after a move

Diffs don't show floating comments: `Reap` landed between
`EvaluatePolicies`'s doc and its function, and no tool cared.
After moving a function, read the destination top to bottom and
check every doc sits on its own function.

— [61c91f7](https://nrtn.dev/catalyst/kpr/commit/61c91f7e4751066aa5a6ebdb4ecb4fdf7b283b21)

## Coverage gaps: name them, don't hide them

Extraction-attributable gaps get topped up (direct `ListPlan`
test, zero-branch messages, failing-store fakes). Environmental
gaps (no live redis, helper packages, deferred scope) get named
as out of scope. The gate is the unit+e2e union; unit-only
shortfalls need sentences, not heroics. And wrong doc claims get
corrected in the open ([caa6c8a](https://nrtn.dev/catalyst/kpr/commit/caa6c8afae6f9304404094a4dfe0e2a41ee5c8a5)), not amended into silence.

— [8650e5c](https://nrtn.dev/catalyst/kpr/commit/8650e5c6d77ad8cad721414fb79ee450b816c682), [caa6c8a](https://nrtn.dev/catalyst/kpr/commit/caa6c8afae6f9304404094a4dfe0e2a41ee5c8a5)

## Never cut a port before its second user

`Collector` cut a slice early earned a YAGNI flag — answered by
the next slice consuming it. Vocabulary first (gc-1 nouns),
groundwork without ports (gc-2 position), the port where a stub
already exists (gc-3), consumption by orchestration (gc-4). The
line between design and cargo cult: cut where a second
implementation already stands, never in anticipation of one.

— [3cf31a8](https://nrtn.dev/catalyst/kpr/commit/3cf31a8e4e0472bd61daae9ae7fbba9bfb4f35c6), [15058c6](https://nrtn.dev/catalyst/kpr/commit/15058c6e8cd2824d517a2302a0fa6444ded3eef4)

## Seams prototype ports

`collectorCommand` (package var, swapped in tests) is a seam:
invisible, global, test-only. `Collector` is the same need made
public: in the signature, usable by any caller, production passing
the real thing explicitly. Ports don't eliminate seams — they push
them to the boundary (`RunCollector` still bottoms out in the
seam, because something must finally call `exec`). A seam used in
one test file stays a seam; the second consumer promotes it.

— [15058c6](https://nrtn.dev/catalyst/kpr/commit/15058c6e8cd2824d517a2302a0fa6444ded3eef4)
