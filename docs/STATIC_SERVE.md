# Serving registry images as static content (deferred)

Question: should kpr serve image content — tag → manifest →
layers → files — as browsable static content, the way
[simple-containers](https://nrtn.dev/sandbox/simple-containers)
packs directories into squashfs, pushes them with custom media
types, and serves them back over HTTP/S3? Verdict: not now;
this doc keeps the pointer so the idea survives.

## Findings

- The read path already exists in pieces: the analysis walk
  resolves repos → tags → revisions → blobs on the local fs,
  and the registry API serves manifests and blobs by digest.
  A static view is a join over those, no new storage.
- simple-containers proves the serving end independently:
  content-addressed artifacts behind an HTTP file browser,
  SPA mode, directory listing. Nothing there needs kpr.
- The missing middle is manifest → file Tree resolution
  (layers are tar diffs, not files; image config names no
  paths). Serving *files* means applying layer stacks, not
  just pointing at blobs.

## Constraint

A static view earns its existence iff kpr itself needs to
read inside images — sentinel inspection, console previews,
content-aware policy. Until a kpr use case names files, it
is a separate tool wearing kpr's clothes. Revisit when the
first such use case lands; borrow the serving shape from
simple-containers, resolve the layer stack here.
