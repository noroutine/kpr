# Console and status

One status model, two renderers. Every card is a `keeper`
struct assembled once; `kpr status <card>` renders it as
plain text, the console renders it as HTML. Neither renderer
recomputes what the model already decided.

- [The model](#the-model)
- [Standing](#standing)
- [Display kit](#display-kit)
- [Cards](#cards)
- [Where CLI and console diverge](#where-cli-and-console-diverge)
- [Adding a card](#adding-a-card)
- [History](#history)

## The model

- Cards live in `internal/keeper`, one file each
  (`storecard.go`, `proofcard.go`, `edgecard.go`,
  `clockcard.go`, `registrycard.go`, `lists.go`,
  `trustcard.go`, `obscard.go`).
- Each card has a struct plus a `Fetch*` assembly function.
  The fetch reads every input the card needs (lock marker,
  identity, fence snapshot, probe), so renderers never do a
  second round trip.
- Kind is the struct type; standing is decided at assembly.
  Renderers translate the standing into words or bulbs, never
  raw bools.

## Standing

The machine code both renderers sign:

- `ok`, `held`, `denied`, `degraded`, `unreachable`,
  `unproven`, `locked`, `closed`, `unavailable`,
  `unchecked`.
- Assembly rules, tested once in `keeper`:
  - deny wins over held;
  - the lock note promotes from meta to standing (locked,
    or degraded on a marker read failure);
  - a local clock is `unchecked`, never `ok`;
  - missing evidence never alarms (unproven, unavailable,
    unknown — never a guess).
- `--json` carries the standing code; CLI text never prints
  it (each card voices its own words instead).

## Display kit

Shared formatting lives in `internal/helpers/human`, so both
renderers spell the same values the same way:

- durations and ages (`Dur`, `Ago`, `ShortAge`, `Age`);
- signed clock offsets (`SignedOffset`);
- byte sizes and percents (`Bytes`, `Capacity`,
  `UsedPercent`).
- `keeper.FormatOutcome` prints one activity ring entry for
  either renderer; `keeper.ActivityTail` (10) caps both
  tails.

## Cards

| Card | `keeper` model | CLI | Console section |
|---|---|---|---|
| Store | `StoreCard` + `Status` counts + proof voice | `status store` | Store |
| Trust | `TrustCard{Standing, Word}` | `trust:` line on `status store` | — (none yet) |
| Registry | `RegistryCard{Standing, URL, Reachable}` | `status registry` | Registry |
| Edge | `EdgeCard{Standing, Open, Deny, Hold, EdgeAddr, RegistryURL}` | `status edge` | Fence |
| Clock | `ClockCard` (raw durations, not words) | `status clock` | Server time |
| Proof | `ProofCard{Standing, Gen, TS}` | `proof:` line on `status` | Sentinel |
| Activity | `ActivityCard{Outcomes, Total}` | `status activity` | Activity |
| Plan | `Status.Plan` rows | — (`kpr plan` owns the human view) | Plan list |
| Observability | `ObsCard{Links}` | `status observability` | Launchpad |

Console-only, no model: Metrics (`/metrics` serves both),
Configuration (`kpr config`'s job), API Endpoints (static
list).

The aggregate `kpr status` is not a card: one posture line
per section (registry, store, edge, clock, plan, activity,
observability) plus the proof line. Counters, proof, and
ages live behind the cards, not in it.

Field notes:

- `StoreCard.Identity` voices the id the lineage verdicts
  judge the served generation against (`unpaired` names
  itself, `unknown` on a read failure, empty on no store —
  the accepted baseline is adopt-time detail and stays out).
- `StoreCard.FsNote` voices filesystem size for the file
  backend, proven stores only.
- `TrustCard.Word` is the verdict's own word (`paired`,
  `foreign`, `rollback?`, `behind`, `unserved`, `unproven`,
  …); renderers voice it as-is, never a translation.
- `EdgeCard.Hold` is present tense (`hold`, not `held`):
  the lease pins writes now.
- `ClockCard` keeps raw durations; `SkewText`/`NoteText`
  voice them. `Now` stays out of every shared struct —
  each renderer stamps its own render time.

## Where CLI and console diverge

- Standing codes: CLI text voices words (fence effects,
  registry reachability, clock state lines); the console
  maps standing to a bulb class (`bulbClass`: deny red,
  held/degraded amber, ok teal, else the default border).
- Trust has a CLI line but no console card yet.
- Plan has a console list but no `status plan` twin.
- Proof has no `status proof` command: the card feeds the
  `status` proof line and the console sentinel section.
- Clock: the CLI prints a `now:` line, the console a
  Server-time card — same stamp-beside-card shape.
- Activity: both cap at the last 10; the CLI indents entries
  with two spaces, the console pads via CSS. The full ring
  keeps the `/api/activity` shape; CLI `--json` carries the
  card instead.
- `--json` is each card verbatim, plus the same extras the
  text carries (clock stamp) — never a subset the text
  outgrows.
- CLI renders assemble the card in a builder, then write
  once: a broken pipe errors, never prints half a card as
  success. Plain text, no ANSI.
- Every `status` command refuses without a backend instead
  of inventing numbers; the bad-backend test pins them all.

## Adding a card

Model first, one slice at a time:

1. Struct + `Fetch*` in `internal/keeper/<card>card.go`,
   standing decided at assembly, with a standing-matrix
   test (facts in, code out). No model lands untested.
2. CLI render in `internal/cli/status_<card>.go`: text +
   `--json` from the model, golden tests, and a row in the
   bad-backend refusal test.
3. Console section: read the model in the page assembly in
   `internal/console/handlers.go`, with a passthrough test.
4. `go test ./...` and `make lint` green, then a Claude
   review per [`REVIEWER_CONTEXT.md`](REVIEWER_CONTEXT.md)
   before the next card starts.

## History

- The cards were built one by one beside a frozen
  `store status`, golden-compared against it, and the
  reference dropped once every fact had a card home.
- `status plan` was dropped: `kpr plan` owns the human
  view, a status twin only duplicated it.
- Gateway/clock parity from `CLI_FUTURE.md` fell out of the
  model (edge and clock cards) and that file dissolved.
