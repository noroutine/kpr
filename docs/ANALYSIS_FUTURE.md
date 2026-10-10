# Registry analysis — future work

Designs for magnitude work that is **not built**. Shipped behavior
lives in `kpr registry analyze` and the layout it walks,
[REGISTRY_LAYOUT.md](REGISTRY_LAYOUT.md). Dangling-reference
designs live in [GC_DANGLING.md](GC_DANGLING.md).

## Contents

- [Tagged/untagged/orphan blob attribution](#taggeduntaggedorphan-blob-attribution)
- [GC profit line](#gc-profit-line)
- [Untagged is arithmetic, not truth](#untagged-is-arithmetic-not-truth)

## Tagged/untagged/orphan blob attribution

Analyze reports blob bytes as one number (632.18 GiB on the
testbed). It cannot say how much of that belongs to tagged
manifests, because the layer links it counts say "blob belongs
to repo" — only manifest bodies say "blob belongs to tag".
There is no cheap approximation: attributing bytes requires
reading every manifest.

The scan is affordable anyway. Manifests are tiny (559–9,535
bytes observed); reading and parsing all ~25k of them is
seconds against the ~48s walk, all local fs under the same
filestore proof, no API calls.

Algorithm:

1. Tagged digests: read all tag `current/link` files → set T.
2. All manifest digests: read all revision links → set R.
   Untagged ≈ R − T.
3. Manifest → blobs: each revision digest names a blob file
   holding manifest JSON. Read it, collect layer + config
   digests, recursing through multi-arch indexes
   (index → children → layers).
4. Attribute blob bytes reachable-wins: reachable from any
   digest in T counts tagged; reachable only from R−T counts
   untagged; on disk but reachable from nothing counts
   orphan — stale residue, the most GC-interesting bucket.

Complications, all manageable:

- Shared base layers force three buckets, not two — a blob
  under both a tagged and an untagged manifest counts
  tagged, otherwise sharing double-counts.
- Index children are usually untagged revisions themselves;
  the walk must follow index→child edges, not just leaves.
- Foreign (URL-mounted) layers and schema-1 manifests skip
  gracefully when no blob file exists — never abort the scan.
- Point-in-time like analyze: a live registry shifts under
  the read.

Slicing, shaped like the walker: (1) manifest-graph builder
with staged-manifest tests; (2) attribution report plus
CLI/docs. This stays out of `analyze` — the fast magnitude
view — and lands as its own detector on the dangling track.

## GC profit line

The attribution's untagged + orphan buckets are the gc
profit, in bytes: blobs reachable only from untagged
revisions plus blobs reachable from nothing are exactly
what `gc --delete-untagged` reaps. So the detector earns a
one-line rendering next to `size`:

```text
gc profit: ~113.38 GiB reclaimable (delete-untagged)
```

(calibrated: 632.18 GiB before gc, 518.80 GiB after, on the
testbed). The non-naive part is the set difference against
the tagged closure — shared base layers survive under a
tagged revision and must not count. Without the flag the
profit is ~0 by construction; print that too, so the flag's
effect is visible instead of implied. Stale under concurrent
pushes, like everything point-in-time; husk/empty-dir
pruning excluded as noise-level bytes.

## Untagged is arithmetic, not truth

`untagged := fs.Revisions - fs.Tags` counts link files, not
reachability, and conflates three populations:

1. True orphans — referenced by nothing. ~0 right after
   `gc --delete-untagged`; gc did its job on these.
2. Index children — per-arch manifests under a tagged
   multi-arch index. Reachable, protected, no tag of their
   own. The bulk at 22,419 revs / 2,950 tags.
3. Superseded tag versions — old `current` links demoted to
   `tags/<tag>/index/` on overwrite. The walk doesn't count
   them, stock gc treats them as roots, so they pin blobs
   forever: the classic tag-overwrite leak.

A surviving `untagged` count after gc is therefore not
evidence gc failed — populations 2 and 3 survive it by
design. The attribution walk splits all three, replacing
the subtraction with reachability. Diagnostic for
population 3 on a live box:

```sh
find /var/lib/registry/docker/registry/v2/repositories \
  -path '*tags*index*link' | wc -l
```

If large, superseded versions deserve their own
`registry ls`-visible count.
