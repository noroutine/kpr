# CLI — future work

Parked ideas, not built. Shipped layout lives in [CLI.md](CLI.md).
One subpackage per command was tried (`internal/cli/gc`) and
reverted: with the run resolving its own reporter, fence, and
clock, the adapter is one literal — no seam worth a package.

## Config rides no field

`deps.Deps.Cfg` is gone: `config.Current()` already names
everything it carried, and tests stage it the same way
production reads it (`config.SetCurrent` + cleanup).
Precedent set with `RegistryBinPath`, `ClockSource`,
`RegistryURL` — resolve at use, never thread. A field that
carries no per-run variation is a seam wearing a port's
clothes (W13). `OpenDeps` still builds from env and installs
exactly once — construction stays, only the carrying went.

Deferred, deliberately: `gc.Deps.Clock`. Production never sets
it (the `nil` fallback to `config.Current().ClockSource()`
always fires), but ~10 gc tests inject `fakes.StubClock` for
skewed (`Off: time.Hour`) and dead (`Err`) clocks — and
`ClockSource()` only derives NTP/HTTPS/local from `TimeMethod`,
with no knob for a fixed clock. Removing the field needs a
config-level test-clock knob (production-reachable time control)
or real-clock tests; either smell is its own decision, not this
slice.
