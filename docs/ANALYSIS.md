# Analysis

`kpr registry analyze` reports registry magnitude: how many
repos, tags, revisions, blobs, and bytes — the numbers that tell
whether a registry is small enough to reason about by hand.

```bash
kpr registry analyze         # five live lines, nothing reprints
kpr registry analyze --json  # exact bytes for scripts
```

The fast catalog view prints first (repos, tags — a rough size
up front), then the slow fs walk counts beneath it. On a
terminal the eight lines repaint in place; on a pipe the catalog
line flushes right after its walk and the rest follows the fs
walk. Read-only and verdict-free: deltas are information, the
command judges nothing. Needs the filestore proof
(`KPR_REGISTRY_CONFIG` names a config with a filesystem
storage root). Point-in-time on a live registry. Designs not
yet built live in [ANALYSIS_FUTURE.md](ANALYSIS_FUTURE.md).

## Contents

- [The eight lines](#the-eight-lines)
- [How it walks](#how-it-walks)
- [Why repositories content is not counted](#why-repositories-content-is-not-counted)

## The eight lines

```
catalog: 600 repos, 17050 tags, 5 sentinels
store  : 599 repos, 17048 tags, 1 sentinel, store status: paired
store Δ: -1 repo, -2 tags, -4 sentinels
fs     : 600 repos, 17050 tags, 5 sentinels
fs Δ   : +0 repos, +0 tags, +0 sentinels
revs   : 24993 revisions, 7943 untagged
blobs  : 55077 blobs, 101173 layer links, 1 upload
size   : 632.18 GiB blobs
```

Counted nouns pluralize (1 repo, 2 repos); participles
(tracked, untagged) and substance labels (GiB blobs) stay
invariant.

- `catalog` — what the API names, sentinel tags apart. The
  API has no endpoints for revisions, blobs, uploads, or
  layer links, so those stay fs-side.
- `store` — the tracked state, a static snapshot of
  `store ls` read before the slow walks: distinct repos over
  everything, tags over everything (like the catalog counts
  them), sentinel rows as a memo, deltas store-minus-API (what
  adoption and sweeping still owe the registry), plus a
  `store status` field — `paired` when the dry verdict holds, or why not:
  `unpaired` (fresh/wiped), `unserved` (paired store, silent
  registry), `unproven` (evidence unusable), `foreign`,
  `rollback?`, `behind` (served generation untracked yet).
  Stats print regardless: the word colors them, never refuses
  them. `store status` headlines the same word.
  registry). Best-effort: a dead backend degrades this line to
  `unavailable` instead of refusing the walk.
- `fs` — what the walk finds, sentinels apart, plus the
  running fs-minus-API deltas, negative while the walk counts
  up, converging on the skew. The sentinel delta is the
  machinery-footprint mismatch: normally +0. Tagless repos tail
  the line as `N husks` when present (names ride `--json` as
  `husk_repos`); the repo count reads net of them, converging
  with the catalog as verdicts land live. `gc` removes them
  armed, orphans go with the next collect.
- `revs` — manifests on disk (every push writes one, tagged
  or not) and `untagged` = revisions minus fs tags, clamped
  at zero. Tags are pointers; revisions are residents. Dead
  pointers tail the line only when present: dangling tag links
  (target revision link absent), read off link contents and
  joined end-exact — point-in-time on a live registry, raw
  numbers, never verdicts.
- `blobs` — blob files, per-repo layer links (links-per-blob
  reads sharing off this line), upload sessions, plus dangling
  layer links (blob data absent) on the same terms.

No dangling tails on a block is itself the signal: the walk
checked every link and found nothing dead. `--json` always
carries `dangling_tags` / `dangling_layers` (zeros included),
so scripts need never parse silence.
- `size` — blob bytes only, at their own unit. Exact bytes
  stay in `--json`.

## How it walks

Two subtrees, `repositories/` and `blobs/`, walk in parallel
and merge at the join — they never overlap. Progress reports
the running merged total; the end is exact. A missing subtree
reads as zeros (blobs can land before any repo exists, and
vice versa). A walk error names its path: magnitude is exact
or refused, never guessed.

## Why repositories content is not counted

Measured on a 600-repo, 17k-tag store, `repositories/` reports
627M under `du -sh` but holds 35.6M of content
(`du -shA` — apparent size): ~9.7M of link files, ~1.2M of
multi-arch index links, ~24.7M of directory entries, one
108-byte stale upload file. The remaining ~591M — 94% — is
block overhead from ~377k files and dirs, which no object
counter can itemize.

Counting that content was tried and removed: weighing every
link costs an lstat per file on each walk and resolves to
~10M next to 632 GiB of blobs — noise that slows the answer
without changing any decision. Blob bytes are the store;
everything else is index and overhead. (`du -shA` on macOS
shows apparent size; GNU coreutils uses `--apparent-size`.)
