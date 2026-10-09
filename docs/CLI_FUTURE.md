# CLI — future work

Parked ideas, not built. Shipped layout lives in [CLI.md](CLI.md).
One subpackage per command was tried (`internal/cli/gc`) and
reverted: with the run resolving its own reporter, fence, and
clock, the adapter is one literal — no seam worth a package.

## Unlock takes Deps

`gc.Unlock` still takes eleven positionals (`api, configPath,
st, rec, ids, rows, clk, timeServer…`) — the shape `gc.Run`
had before `Deps`/`Options`/`Accepts`. A future move folds it
into the same bundle: store roles, proofs, and acceptances,
nothing positional past the writer.

## Config rides no field

`gc.Deps.Clock` and `deps.Deps.Cfg` are config by another
name: `config.Current()` already names the clock source and
everything `Cfg` carries, and tests stage it the same way
production reads it. Precedent set with `RegistryBinPath`,
`ClockSource`, `RegistryURL` — resolve at use, never thread.
A field that carries no per-run variation is a seam wearing
a port's clothes (W13).
