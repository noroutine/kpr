# Registry analysis — future work

Designs for magnitude work that is **not built**. Shipped behavior
lives in `kpr registry analyze` and the layout it walks,
[REGISTRY_LAYOUT.md](REGISTRY_LAYOUT.md). Dangling-reference
designs live in [GC_DANGLING.md](GC_DANGLING.md).

## Contents

- [Tagged/untagged/orphan blob attribution](#taggeduntaggedorphan-blob-attribution)

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
