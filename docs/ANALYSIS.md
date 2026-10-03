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
terminal the five lines repaint in place; on a pipe the catalog
line flushes right after its walk and the rest follows the fs
walk. Read-only and verdict-free: deltas are information, the
command judges nothing. Needs the filestore proof
(`KPR_REGISTRY_CONFIG` names a config with a filesystem
storage root). Point-in-time on a live registry. Designs not
yet built live in [ANALYSIS_FUTURE.md](ANALYSIS_FUTURE.md).

## Contents

- [The five lines](#the-five-lines)
- [How it walks](#how-it-walks)
- [Why repositories content is not counted](#why-repositories-content-is-not-counted)

## The five lines

```
catalog: 600 repos, 17050 tags
fs     : 600 repos, 17050 tags, Δ repos: +0, Δ tags: +0
revs   : 24993 revisions, 7943 untagged
blobs  : 55077 blobs, 101173 layer links, 1 uploads
size   : 632.18 GiB blobs
```

- `catalog` — what the API names. The API has no endpoints
  for revisions, blobs, uploads, or layer links, so those stay
  fs-side.
- `fs` — what the walk finds, plus the running fs-minus-API
  delta, negative while the walk counts up, converging on the
  skew.
- `revs` — manifests on disk (every push writes one, tagged
  or not) and `untagged` = revisions minus fs tags, clamped
  at zero. Tags are pointers; revisions are residents.
- `blobs` — blob files, per-repo layer links (links-per-blob
  reads sharing off this line), upload sessions.
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
