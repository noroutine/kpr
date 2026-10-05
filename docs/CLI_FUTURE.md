# CLI — future work

Command moves out of the `cli` package that are **not built**.
Shipped layout lives in [CLI.md](CLI.md).

## One subpackage per command

`gc` moved first (`internal/cli/gc`, imported as `gccmd`):
its flags, help, `RunE`, rendering, acceptances, and fence
factory live behind the exported `Cmd`, registered from the
parent root. Everything it needs from the shared wiring comes
from [`internal/cli/deps`](../internal/cli/deps/deps.go) —
commands never import the parent (import cycle), and never
each other.

Remaining, in dependency order (shared consumers first, so no
temporary cross-imports between subpackages):

1. `store` (lock/unlock/adopt/backfill ride along) — drops
   the `gccmd.GcDryRun` use when backfill moves with it.
2. `registry` (analyze/ls) — catalog reads, no store writes.
3. `keeper` surface (status/plan/reap/sweep) — pure reporters
   plus directed runs.
4. `serve`, `env`, `license` — serve wires edge/console/app
   and stays last (most assembled dependencies).

Each move keeps its tests beside it (the `gcrun` import-alias
pattern covers the use-case package sharing the command's
name) and keeps the parent registering the exported `Cmd`.
`deps` gains nothing per move — if a move wants something
from the parent that `deps` lacks, that something was
command code wearing shared clothes.
