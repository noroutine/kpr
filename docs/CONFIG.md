# internal/config

Developer reference for `internal/config`'s structure: what belongs where,
and the steps to add something new in each dimension. User-facing
configuration (which `KPR_*`/`OTEL_*` variables exist and what the flags
default to) is documented by the `EnvVars` list and the cobra command help
in `internal/cli`; this page documents the machinery behind them.

The rule for the whole package: each fact has exactly one place it is
written down, and `internal/cli` seeds flag defaults from the resolved
`Config` instead of maintaining its own copy.

## The three dimensions (plus argv)

| File | Answers | Mutable at runtime? | Set by |
| --- | --- | --- | --- |
| `config.go` | What did the environment ask for? | Yes — one atomic swap (`Current`/`SetCurrent`) | `KPR_*`/`OTEL_*` env vars layered with cobra flag values, or a test's `Builder` |
| `compiletime.go` | What binary is this, and which build? | No — vars, but only ever set once, before `main` runs | `-ldflags -X` at build time |
| `runtime.go` | Where does this process's own executable live, and what platform was it built for? | Two ways — see below | `os.Executable`/`os.UserHomeDir`, or the standard `runtime` package |

Argv itself is deliberately **not** a fourth dimension here: CLI flags
live on the cobra commands in `internal/cli` (cobra parses argv and
renders `--help` from the command structs, so a parallel `FlagDefs`
registry would be a second source of truth, not a single one). The
contract the other way is strict: flag *defaults* seed from a
`FromEnv`-built `Config`, and `serve` layers the parsed flag values back
onto the `Builder` before `SetCurrent` — so the default value is written
down exactly once, in this package.

## Runtime settings — `config.go`

The dimension for anything an operator can override via environment
variable, plus the handful of tunable constants that have no override at
all (server timeouts) but still shouldn't be scattered package-level
`const`s next to whatever reads them.

**Shape:** `Config` is a plain struct with one field per setting.
`Builder` builds one via a fluent `With*` setter per field, plus
`FromEnv()` which resolves every `KPR_*`/`OTEL_*` var into the matching
field. `Build()` never fails outright — an invalid env value falls back
to its default, recorded in a companion `*Warning` field (e.g.
`ManagementPortWarning`) for startup to log once.

**Access:** production code calls `NewBuilder().FromEnv().Build()` once
at startup (in `internal/cli`: flag defaults seed from it, `serve`
layers flag values on top) and installs it with `SetCurrent`; every
consumer elsewhere reads `Current().Field` — never a package-level var of
its own. `SetCurrent` swaps one `atomic.Pointer[Config]`, so a reader
never observes a half-updated config, and `Current()` is never nil (an
`init()`-installed default config backs it before anything calls
`SetCurrent`).

**Tests** build their own `Config` (`NewBuilder()`, which never touches
the real environment) and install it for the test's scope via the
one-liner `SetCurrent` already returns:

```go
t.Cleanup(config.SetCurrent(config.NewBuilder().WithRedisAddr("localhost:6380").Build()))
```

### Adding a new env var

1. Add an `Env*` const to the grouped block near the top of `config.go`,
   in the group matching what it controls (server bindings, state
   backend, observability) — add a new group if none fit.
2. Add its `EnvVar{Name, Description}` entry to `EnvVars`, **in the
   matching group, in the same relative position** as the const above.
   `EnvVars` is what documents the variable for operators — the const's
   own doc comment is for godoc, not for users, so don't rely on it alone.
3. Add the field to `Config`, a `With*` setter on `Builder`, and resolve
   it in `FromEnv()`. Decide fallback-vs-warning for a bad value up
   front: ports fail soft (default + `*Warning`); most strings have no
   invalid state at all.
4. Add the const to the explicit list in
   `TestEnvVarsDocumentsEveryEnvConst` (fails if const block and
   `EnvVars` drift apart) and a `FromEnv` test for the new var's
   parsing/fallback behavior. If a cobra flag should expose it, seed the
   flag default from the resolved `Config` in `internal/cli` and layer
   the parsed value back in `serve` — never a literal default in both
   places.

### Adding a plain tunable constant (no env override)

Add it to the second `const` block in `config.go` (the one with no
matching `Env*`), seed the `Config` field from it in `defaultConfig()`,
and add a `With*` setter so tests can shrink it. Server timeouts are the
current residents: nothing reads the bare const outside this package.

## Compile-time metadata — `compiletime.go`

The dimension for "what binary is this, and which build" — as distinct
from `config.go`'s env-driven settings, which can change between runs of
the *same* binary.

**Shape:** `Name` is a plain `const`. `Version`, `Commit`, `BuildTime`
are package-level `var`s specifically because `-ldflags -X pkg.Var=value`
can only overwrite a package-level string **var**, never a `const` or a
struct field — this is the one dimension here that *has* to be scattered
top-level vars rather than `Config` fields, because the linker writes to
them before `main` (and thus before any `Builder`/`Current()` machinery)
ever runs. All three default to placeholder values
(`"dev"`/`"unknown"`/`"unknown"`) so `go run ./cmd/app` and an ad hoc
`go build` (no ldflags) still print something sensible.

**Access:** `VersionString()` renders the single-line identity string;
`PrintVersion(w io.Writer)` writes it with a trailing newline. Consumer
sites (`--version` via cobra, startup log lines, the console) call these
— never a hand-built `"kpr " + Version` at the call site.

**Wired by:** the `-X` paths in `Makefile`'s and `justfile`'s `LDFLAGS`,
`Dockerfile`, and the example in `BUILD.md#build-flags`. If this package
or these var names ever move again, all of those need updating too.
Verify any such change with a real build:

```bash
go build -ldflags "-X nrtn.dev/catalyst/kpr/internal/config.Version=vX.Y.Z" -o /tmp/kpr ./cmd/app && /tmp/kpr --version
```

### Adding a new compile-time value

Add a `var` (not `const`) with a placeholder default next to the existing
three, wire its `-X` path into `Makefile`/`justfile`/`Dockerfile`, and
fold it into `VersionString()` (or wherever it's meant to surface) if
it's user-facing.

## Process/OS runtime facts — `runtime.go`

The dimension for facts about the outer environment this specific process
happens to be running inside right now — where its own executable lives
relative to `$HOME`, and which platform it was built for. Distinct from
`config.go`'s settings (those come from the environment *asking* for
something) and from `compiletime.go`'s metadata (fixed at build time, not
derived from the OS) — this is neither. It splits into two shapes,
depending on whether anything ever needs to override the fact in a test:

**Derived fresh, no `Config`-style override — `InstalledBinaryPath`/
`HomeRelativePath`:** `InstalledBinaryPath()` resolves `os.Executable()`
to `$HOME/...` form (falling back to `Name` if `os.Executable` itself
fails) so help text and install hints print a copy-pasteable path
instead of `go test`'s own temp binary path. Neither caches or has a
`Config`-style field — nothing here is expensive enough or called often
enough to be worth resolving once and storing. Their two real OS calls
(`os.Executable`, `filepath.Abs`) are still wrapped as ordinary
**behavior seams** — `osExecutable`/`filepathAbs` — purely so their
error-fallback branches are actually reachable by a test; that's a
different concern from the `GOOS`/`GOARCH` override below and uses the
plain seam pattern, not a `Builder`/`Current` pair.

**Swappable for tests — `Runtime`/`RuntimeBuilder`/`CurrentRuntime`/
`SetCurrentRuntime`:** `GOOS`/`GOARCH` are exactly the kind of fact
several packages branch on but that only one real value ever exists per
binary. A bare `var GOOS string`, reassignable by any test that imports
the package, is the "hatchet you cut yourself with": nothing stops
production code from writing to it, and nothing stops one test's override
from leaking into the next if a cleanup is missed. So this dimension gets
its own copy of `config.go`'s Builder/Current/SetCurrent shape instead —
deliberately **not** folded into the main `Config`/`Builder`, to keep
"what the environment asked for" and "what platform this is" as separate
concerns.

Every caller reads `config.CurrentRuntime().GOOS`/`.GOARCH` — never the
standard `runtime` package directly, and never `Runtime`'s fields without
going through `CurrentRuntime()`. A test overriding a platform-specific
branch does the one-liner:

```go
t.Cleanup(config.SetCurrentRuntime(config.NewRuntimeBuilder().WithGOOS("windows").Build()))
```

This does **not** extend to code whose branch exists because the real
OS's syscall semantics differ — faking `GOOS` there exercises the code
path but proves nothing about the real platform behavior it exists for;
that kind of test still guards with the real `runtime.GOOS` and skips
itself where it must.

### Adding a new runtime fact

If it's cheap to derive, only ever needed for a log-line style one-off,
and nothing will ever need to fake it in a test, add a function
alongside `InstalledBinaryPath`/`HomeRelativePath` — no caching, no
struct field. If something *will* need to override it to exercise
another platform/environment's branch — the `Runtime` situation — add
the field to `Runtime`, a `With*` setter to `RuntimeBuilder`, seed it in
`defaultRuntime()`, and read it everywhere via `CurrentRuntime()`. If
neither fits — it's read repeatedly across a run and has a real env
override, or it's an ordinary tunable with no OS/platform angle at all —
it's probably not this dimension at all; reconsider whether it's
actually a `config.go` setting.

## Test seams vs. config values

Not everything that looks like a "mockable knob" belongs in this
package. Short version of the rule:

- If overriding the var's value could ever make a test slower, flakier,
  or touch the filesystem/network/a subprocess, it's a **behavior seam**
  and stays local to the package that owns the real implementation it
  wraps, with an `is a seam for tests:` doc comment (`osExecutable` and
  `filepathAbs` in `runtime.go` are this package's own examples).
- If replacing it only ever changes what a comparison or a rendered
  string looks like, it's a **config value** and belongs in one of the
  dimensions above — almost always `config.go` (a `Config` field plus a
  `With*` setter, and an `Env*` const plus `FromEnv()` resolution if it
  has a real env override).
