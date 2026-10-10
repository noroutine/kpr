# Garbage collection — future work

Designs for gc work that is **not built**. Shipped behavior lives in
[GC.md](GC.md). Dangling-reference designs live in
[GC_DANGLING.md](GC_DANGLING.md).

## Contents

- [Per-repo collection](#per-repo-collection)

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
