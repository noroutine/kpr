# CLI

`kpr` commands, one subpackage each (all moved or moving —
see [`docs/CLI_FUTURE.md`](CLI_FUTURE.md)). Shared wiring
lives in [`internal/clideps`](../internal/clideps/deps.go):
`Deps`/`OpenDeps` (config, state, registry client),
`OpenStore`/`BuildStore`/`StoreName`,
`ResolveStoreBackend`, `ClockSource`. Commands never wire
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

First command in its own subpackage
(`internal/cli/gc`, imported as `gccmd`): flags, help, `RunE`,
rendering (`renderGCEvent`), acceptances (`gcAccepts`), the
fence adapter factory (`newFenceControl`), the stock binary
path (`RegistryBinPath`). `GcDryRun` stays exported: backfill
shares the run-mode semantics until it moves too.

Wiring order inside `RunE`: open deps → arm the run (flag or
env, never both, never neither — dry-run is the absence of
`Armed`) → resolve backend → fence → `gc.Run`. Refusals name
their remedy; usage never prints on refusal.
