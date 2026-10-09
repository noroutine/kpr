# CLI

`kpr` commands, one subpackage each (all moved or moving —
see [`docs/CLI_FUTURE.md`](CLI_FUTURE.md)). Shared wiring
lives in [`internal/cli/deps`](../internal/cli/deps/deps.go):
`Deps`/`OpenDeps` (config, state, registry client),
`OpenStore`/`BuildStore`/`StoreName`,
`ResolveStoreBackend`. Commands never wire
each other: the parent only registers their exported `Cmd`.

```
kpr
├── status                    banner + counters as text
├── plan add|remove|discard   pending candidates
├── reap [policy]             run a reaping policy once
├── sweep                     directed sweep pass
├── gc                        garbage-collect (dry-run default)
├── store ls|inspect|rm|status rows, plus lock|unlock|adopt|backfill
├── registry analyze|ls       catalog reads
├── serve                     edge + console + app
├── env                       effective KPR_* variables
└── license                   print the GPLv3 license
```

## gc

A plain command file (`internal/cli/gc.go`): flags, help,
`RunE`, acceptances (`gcAccepts`). The stock binary path
lives in config (`KPR_REGISTRY_BIN_PATH`, default
`/bin/registry`).

Wiring order inside `RunE`: open deps → arm the run (flag or
env, never both, never neither — dry-run is the absence of
`Armed`) → `gc.Run` with the store in all four roles inline,
`Options`, and `gcAccepts`. The run renders and resolves
reporter, fence, and clock itself. Refusals name their
remedy; usage never prints on refusal.
