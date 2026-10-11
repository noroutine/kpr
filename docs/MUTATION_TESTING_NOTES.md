# Mutation testing notes: where gremlins is blind

> Human field notes, not agent instructions. Agents: every item
> below was earned by a hand-flip on its own run — never add a
> `NOTE(mutants)`, skip a test, or call a mutant equivalent on
> the authority of this list alone. The list tells you where to
> look; only a failing flip tells you what is true.

Gremlins' "covered" means "a coverprofile block starts on the
mutant's line", not "the line executes". Go attributes a
multi-line statement to its first line (or its body), so any
mutant sitting anywhere else on the statement is structurally
invisible: covered code reported NOT COVERED. All observed
cases were false negatives, never false positives.

## Shapes

- `case` condition lines. The case body gets blocks; the
  condition doesn't. Seen on: adopt's dispatch, fossil and
  delete switches (`storeops/adopt.go`); sweep's re-read,
  Prover and delete switches plus untag (`sweep/`); gc's
  collect dispatch (`gc/run.go`); registryfs's dir and link
  switches (`registryfs/analyze.go`); `Capacity`'s bands
  (`helpers/human`).

  ```go
  switch {
  case cur.ID != "" && cur.ID != pay.ID: // <- mutant here; no block
      return repair(...)                  // <- blocks here; executes
  ```

- Executed guards with no attributed block. Backfill's
  sentinel guard (`backfill/backfill.go`): its block shows
  count 5 in the profile, yet the mutant on it read NOT
  COVERED — and flipping the guard fails the clamp test
  loudly. Same blindness, no case in sight.

  ```go
  if strings.HasPrefix(repo, SentinelPrefix) { // <- executes 5x; still flagged
      ...
  }
  ```

- Split conditions. The block starts at the `if` line, so a
  mutant on the continuation reads NOT COVERED. Seen on:
  sweep's Prover check, gc's dispatch legs, the storecard
  switch case.

  ```go
  if err := fenced(func() error { // <- block starts here
      return collect(...)
  }); err != nil { // <- mutant here; no block
  ```

- Declarations. `const` blocks, `var x = a + b`, multi-line
  struct literals execute at init but emit no blocks. Seen
  on: root's `Long` concat, the registry Accept-types
  concat, TTL consts, the NTP epoch, `ProofStaleAfter`.

  ```go
  Long: `kpr (keeper) fronts ...` +
      config.LicenseBanner(), // <- mutant here; no block, runs at init
  ```

- Helper packages without coverpkg. `fakes` legs execute only
  from other packages' tests; a per-path profile shows zero.
  A `--coverpkg` run over the module shows them executing.

  ```sh
  # per-path: fakes legs read NOT COVERED
  go test ./internal/testing/fakes/ -coverprofile=f.cover
  # module-wide: the same legs execute via gc/storeops/unlock suites
  go test ./internal/... -coverpkg ./internal/testing/fakes -coverprofile=f.cover
  ```

## How they were closed

Each site got one of two endings, both recorded at the site:

- Flip-proven kill: the mutant runs and the suite catches it
  loudly (a new pin test where none existed, e.g. the lease
  PX assertion, the clean-root preview silence, the
  delete-outcome classification). The NOT COVERED was pure
  mislabeling.
- Proven equivalent or untestable: sort comparators on unique
  keys, clamp-to-self, timing bounds, mid-walk races. The
  NOTE states the proof, not just the verdict.
