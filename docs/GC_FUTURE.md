# Garbage collection — future work

Designs for gc work that is **not built**. Shipped behavior lives in
[GC.md](GC.md). Dangling-reference designs live in
[GC_DANGLING.md](GC_DANGLING.md).

## Contents

- [Per-repo collection](#per-repo-collection)
- [Token-auth registries](#token-auth-registries)

## Per-repo collection

The stock collector marks globally: every repository in the store,
every run. There is no way to tell it "collect only this repo",
and kpr inherits that — `kpr gc` has no scoping flag because
there is nothing to pass one to.

This matters most for blast radius. An operator who wants to
reclaim space from one noisy CI namespace has to collect
everything, which means the whole store pays the mark-phase cost
and the whole store is exposed to whatever the run gets wrong.

One repo already works around it, and the workaround is the clue
to a general design: kpr's own sentinel repo keeps its history
bounded without gc scoping at all. Each generation carries a
tracked row, keep-N marks the overflow, the sweeper deletes those
manifests by digest, and an ordinary global collect reclaims the
blobs that are now unreferenced. Tag-level retention does the
scoping; gc stays global and dumb. See [SENTINELS.md](SENTINELS.md).

Whether that generalises — per-repo retention driving a global
collect, rather than a scoped collect — is the open question. A
genuinely scoped collect would need our own mark phase, which is
the same objection that keeps online GC out.

## Token-auth registries

kpr is scoped to anonymous and basic registries today. Against a
token-issuing registry, every kpr-owned call — enumeration,
sentinel, proofs — 401s.

The fix is client-side only; kpr never verifies JWT:

1. Read the issuer realm from the 401 `Bearer` challenge.
2. Present the same user+password pair to it.
3. Fetch per-scope tokens (`registry:catalog:*`, per-repo `pull`).
4. Retry with `Bearer`.

One credential form covers both schemes, and the stock collector
binary is unaffected — it takes its own auth from registry config.

**Sharp edge:** the sentinel classifies mode via upload-initiate,
which needs a *push*-scoped token on the probe repo. Without one,
classification must refuse rather than guess. Likewise a loud
refusal when the issuer won't grant catalog scope.
