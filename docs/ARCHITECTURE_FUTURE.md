# Architecture — future work

What is open or coming. [ARCHITECTURE.md](ARCHITECTURE.md)
describes what ships today.

## Open, in no order

| Item | Status |
| --- | --- |
| keep-N tuning surface (`--last`, `--include`) | declined — N stays 10, with `--exclude` |
| Real partial-upload detection (bounded manifest reads) | open |
| Sweep live-stages transport (polling vs websocket) | deferred — the vocabulary and keys are the contract |
| Registry metrics as a GC-readiness signal (storage pressure before collecting) | noted, not scheduled |
| Tag-release flow (image push + Forgejo release) | unverified |
