# AGENTS.md

kpr (keeper) — lightweight companion sidecar for an OCI distribution
registry. Single Go binary, `just` (or `make`) toolchain, CI on Forgejo.
See `docs/goals.md` for what the project is and where it is going.

## Tests

Run the full suite:

```bash
just test
```

`make test` works identically — every recipe exists in both files, keep
them in sync when adding a new one. Plain `go test ./...` also works.

Coverage (statement-based; Go has no branch-coverage mode) with the
summary printed to the terminal plus `coverage.out`/`coverage.html`:

```bash
just coverage
```

Reprint the table from the last run without re-running tests:

```bash
just coverage-report
```

Benchmarks (no tests, measurements only):

```bash
just bench
```

There are no compose-based test fixtures yet. When integration tests
against the dev stack (`docker-compose.yml`: kpr, redis, registry) are
added, they must skip gracefully when the fixtures are not running and
build every fixture address from an env-exported host — never hardcode
`localhost` (inside CI job containers the fixtures are siblings whose
published ports live on the host, so container localhost never works).
Fixture files live flat next to the compose file; do not add another
folder for test containers.

## CI workflows

Workflows live in `.forgejo/workflows/` and call `just`/`make` targets —
never inline commands or complex `run:` scripts. All logic belongs in the
`justfile`/`Makefile` so it runs identically locally and in CI and stays
reusable across workflows. Inline workflow logic is a last resort, only
where reuse with other CI is super unlikely. Environment setup
(installing Go itself, tool binaries) may use inline `run:` steps.

Actions resolve through the Forgejo mirror (`data.forgejo.org`), which
lags GitHub: pin only action versions whose tags (and their transitive
action dependencies) exist on the mirror. `upload-artifact`/`download-artifact`
v4+ need the GHES artifact service, which the runners lack — do not use
them; print reports (checksums, coverage tables) to the job log instead.

## Forgejo access for agents

API base is `https://nrtn.dev/api/v1`, repo is `catalyst/kpr`.
Authenticate with the token in `.forgejo_token` (repo root, gitignored,
`0600`). Never print, log, or commit the token — pass it via a curl
config file:

```bash
printf 'header = "Authorization: token %s"\n' "$(cat .forgejo_token)" \
  > /tmp/fj_curl && chmod 600 /tmp/fj_curl
curl -K /tmp/fj_curl https://nrtn.dev/api/v1/user
```

Useful endpoints: `/repos/catalyst/kpr/actions/runs?limit=N`
for recent runs (match by commit title), `/actions/runs/{id}` and
`/actions/runs/{id}/jobs` for status, `/actions/jobs/{job_id}/logs`
for full job logs. When babysitting a push: find its runs, poll job
status until terminal, pull logs on failure, fix forward, delete the
temp config file when done.

## GitHub access for agents

Automation that talks to the GitHub API authenticates with the token in
`.github_token` (repo root, gitignored). Same handling as the Forgejo
token: never print, log, or commit it. Export it when a command needs it:

```bash
export GITHUB_TOKEN="$(cat .github_token)"
```

The token needs **Pull requests → Read and write** on the target
repo (reads work with less, closes 401 without it). If writes 401
despite that, check the token is actually exported in the current
shell and authorized for SSO if the org enforces it.

## Test documentation (required on new tests)

Every new test gets documentation **in the language's idiomatic doc
position** (see table), written as a use case — not a restatement of the
test name or the fields it touches.

State three things in a few sentences:

1. **Intent** — use case of the test, description of the tested scenario
2. **Expected** — the visible outcome (phase, event, resource
   created/deleted, requeue, rendered UI, returned value, or other
   observable behavior)
3. **If it fails** — the production lie or harm (stale state, incorrect
   access results, misleading UI, silent retry storm, data loss, or
   another user-visible regression)

Do **not** prefix with the test name (it is already on the next line).
Do **not** narrate code (`AppliedAt is nil so RequeueAfter is 1m`).
Speak the situation directly. Avoid repetitive filler openings such as
"This covers", "This tests", or "This verifies"; start with the user or
system scenario instead.

Keep every documentation line within 120 columns; wrap prose manually —
formatters (`gofmt`, `black`, `rustfmt`, `prettier`) do not wrap comments.

| Language   | Doc position                                              |
| ---------- | --------------------------------------------------------- |
| Go         | `//` godoc immediately above `func TestXxx`               |
| Python     | Docstring as the first statement inside `def test_...`    |
| Java       | Javadoc `/** ... */` immediately above the `@Test` method |
| Rust       | `///` immediately above the `#[test]` function            |
| TypeScript | JSDoc `/** ... */` immediately above `it(...)`/`test(...)`|

When adding documentation to existing tests, or overhauling/auditing
their comments, preserve and integrate the useful technical context
already there. Keep the original rationale, edge cases, invariants, and
regression history; do not replace detailed background with a generic
use-case summary. Rephrase that context into this style where
necessary: explain why the behavior matters, but still avoid
field-by-field code narration, filler openings, and full test names.
