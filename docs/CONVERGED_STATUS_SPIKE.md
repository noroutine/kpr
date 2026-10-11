# Converged status spike

One status model, two renderers. The console assembles three
cards privately (`keeperSnapshot`, `storeSnapshot`,
`sentinelSnapshot` in `internal/console`) while `kpr store
status` assembles its own in `internal/cli/store_status.go`.
This spike converges them: every card is a `keeper` struct,
`kpr status <card>` renders it as text, the console as HTML.
Absorbs `docs/CLI_FUTURE.md` (gateway/clock parity falls out
of the model — dissolved here).

Rule: `store status` is frozen as the reference. The spike
does not touch it except cosmetic changes the spike itself
forces (shared consts, moved helpers — same output). Each
new `status <card>` is built in parallel from the card
models, golden-tested against the reference; `store status`
drops in a separate change once the cards read identical.

## Placement

Cards live in `keeper` (`cards.go`), display in `human` —
no new package, no `keeper` subpackage. `keeper` already is
the read-side store use cases (plan, policy, reap, ghosts,
`Status`) that both renderers import; three small structs
with no independent lifecycle don't earn a ports tree (the
`fence` impurity-note rule). Not `observability`: that name
already means the metrics/traces/logs overlay
(`docs/OBSERVABILITY.md`) — status snapshots sharing it
would collide on every future conversation about either.

## Model: kind and standing

Kind is the struct type. Standing is a machine code set once
at assembly; words and bulbs are composed at render, per
presentation. Neither renderer touches raw bools.

```go
type Standing string

const (
    StandingOK          Standing = "ok"
    StandingHeld        Standing = "held"
    StandingDenied      Standing = "denied"
    StandingDegraded    Standing = "degraded"
    StandingUnreachable Standing = "unreachable"
    StandingUnproven    Standing = "unproven"
    StandingLocked      Standing = "locked"
    StandingClosed      Standing = "closed"
    StandingUnchecked   Standing = "unchecked"
)
```

Console maps standing → bulb class (deny red, held/degraded
amber, ok teal); CLI prints the standing-derived loud line
(caps + bound, plain text, no ANSI — none allowed). `--json`
emits the struct, so API consumers get the machine standing
free. `Now` stays out of shared structs: server and CLI each
stamp their own render time.

## Cards

| Console section | `keeper` model | Command |
|---|---|---|
| Keeper (counters) | `Counters` (existing `Status` counts) | `status` aggregate header |
| Registry | `Registry{Standing, URL, Reachable}` | `status registry` |
| Gateway | `Gateway{Standing, Open, Deny, Held, EdgeAddr, RegistryURL}` | `status gateway` |
| Clock | `Clock{Standing, Method, Server, Skew, Tolerance}` (raw durations, not words) | `status clock` |
| Plan | `Status.Plan` rows (mutations stay `plan add/rm`) | console only — no `status plan` twin (`kpr plan` owns the human view) |
| Store | `Store{Name, Detail, Healthy, Standing, LockNote, FsNote}` + `Status` counts + proof voice | `status store` |
| Sentinel | `Proof{Standing, Gen, TS}` | proof line on `status` (already lives there) |
| Activity | `Activity{Lines, Total}` (tail; full ring keeps `/api/activity` shape) | `status activity` |
| Observability | `Obs{Links}` (configured UI backends; empty reads unavailable) | `status observability` |

Console-only, no model: Metrics (`/metrics` serves both),
Configuration (`kpr config`'s job), API Endpoints (static
list).

Standing rules: deny wins over held; lock note promotes from
meta to standing; local clock is `unchecked`, never `ok`;
unproven fs-note suppression and nil-store degradation stay
as today, now tested once in `keeper`.

## Milestones

- **0 — display kit → `human`.** `SignedOffset`
  (ex-`formatOffset`), `ByteSize` (ex-`fmtBytes`), `Percent`
  (ex-`usedPercent`) + `Standing` type. Truth-table tests.
- **1 — `status store`.** `storeSnapshot` → `keeper.FetchStoreCard`;
  `fsSizes` core moves (`syscall` probe stays wherever
  platform files force). Golden pins today's card.
- **2 — proof.** `sentinelSnapshot` → `keeper.FetchProofCard`;
  feeds console section + `status` proof line. No new command.
- **3 — `status gateway`.** Edge posture + would-deny lock read
  move; `deny_engage` voicing untouched.
- **4 — `status clock`.** `clockSnapshot` → `keeper`
  (env-staged: `TimeMethod=local` pins no-probe, `httptest`
  date server pins skew). Standing-matrix test.
- **5 — plan + activity.** `Activity` card + console plan
  list, one formatter with padding flag (`activityLine` ×
  `renderOutcome` merge); `/api/activity` and `--json`
  shapes converge. (No `status plan` twin: `kpr plan` owns
  the human view — dropped as garbage.)
- **6 — console thin.** Handlers keep HTTP/templates;
  `keeperData` dies (page assembles from cards with only
  template shaping in between; `storeData`/`sentinelData`
  never existed); per-card passthrough tests. (`store status`
  drop stays separate, golden diff before/after.)

Each milestone follows the same shape, model first:

1. Testable card model in `keeper`: struct + assembly with a
   standing-matrix test (facts in → code out), no renderers
   yet. No model lands untested — the model is the contract
   both renderers sign.
2. CLI render: text + `--json` from the model, golden-tested
   (against the `store status` reference where one exists).
3. Console render: section reads the model, passthrough test.
4. Suite + lint green, then Claude review per slice (see
   [`docs/REVIEWER_CONTEXT.md`](REVIEWER_CONTEXT.md)) — next
   milestone starts only after the review lands.

New commands never alias the reference — parallel `status
<card>` beside frozen `store status`, compared by goldens,
reference dropped separately.

## Absorbed from CLI_FUTURE

Gateway line (open/deny/held + `EdgeAddr → RegistryURL`,
disabled renders closed) and clock line (`method server
skew`, bounded probe, unreachable-never-slow) are milestones
3–4, not separate work. `--json` parity and degrade-don't-
refuse are the model rules above.
