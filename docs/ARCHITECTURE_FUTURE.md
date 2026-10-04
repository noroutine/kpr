# Architecture — future work

What is open or coming. [ARCHITECTURE.md](ARCHITECTURE.md)
describes what ships today.

## Open, in no order

| Item | Status |
| --- | --- |
| keep-N tuning surface (`--last`, `--include`) | declined — N stays 10, with `--exclude` |
| Bind the fs proof to the served endpoint (sentinel generation read off the local root must equal the served one) | open — retires the unbound-fs-witness limitation |
| Close the mark-to-delete race (readonly fence across deletes, or push-epoch CAS; equivalently sweep as a gc phase under such a fence) | open, deferred until bitten — the re-read narrows it, harm is a retryable dangling tag |
| One torn-row rule across backends (file refuses, redis skips today) | open — needs a `store.Store` contract case pinning it |
| Real partial-upload detection (bounded manifest reads) | open |
| Sweep live-stages transport (polling vs websocket) | deferred — the vocabulary and keys are the contract |
| Cut presenter adapters out of command bodies (pure render fns like `backfillLines`; port tests assert data, thin goldens assert strings) | open — `resolveBackfillSink` is the first cut |
| Registry metrics as a GC-readiness signal (storage pressure before collecting) | noted, not scheduled |
| Tag-release flow (image push + Forgejo release) | unverified |
