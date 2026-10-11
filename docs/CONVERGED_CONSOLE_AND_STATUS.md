# Console and status <!-- omit in toc -->

One status model, two renderers. Every card is a `keeper`
struct assembled once; `kpr status <card>` renders it as
plain text, the console renders it as HTML. Neither renderer
recomputes what the model already decided.

## Contents <!-- omit in toc -->

- [The Model: Structs in keeper, Words in Renderers](#the-model-structs-in-keeper-words-in-renderers)
  - [Card: A Struct Plus Its Fetch](#card-a-struct-plus-its-fetch)
  - [Standing: The Card's Judged Position](#standing-the-cards-judged-position)
- [Display Kit: Shared Presentation Primitives](#display-kit-shared-presentation-primitives)
- [Cards: Every Card and Its Two Homes](#cards-every-card-and-its-two-homes)
- [`kpr status`: The Aggregate, Not a Card](#kpr-status-the-aggregate-not-a-card)
- [Where CLI and Console Diverge](#where-cli-and-console-diverge)
- [Adding a Card: Model First, One Slice at a Time](#adding-a-card-model-first-one-slice-at-a-time)

## The Model: Structs in keeper, Words in Renderers

### Card: A Struct Plus Its Fetch

A card is a struct plus the `Fetch*` that assembles it:
facts in, standing decided, ready for either renderer. The
fetch does every read the card needs — lock marker,
identity, fence snapshot, probe — so renderers never do a
second round trip. Cards live in `internal/keeper`, one
file each:

- one file per card:
  - `storecard.go`
  - `proofcard.go`
  - `edgecard.go`
  - `clockcard.go`
  - `registrycard.go`
  - `trustcard.go`
  - `obscard.go`
- shared:
  - `lists.go` (activity)

Kind is the struct type, nothing more — `StoreCard`,
`EdgeCard`, `ClockCard` name which card this is. Each kind
defines how the standing is presented: the same `ok` voices
`ready` on a store and a skew on a clock.

### Standing: The Card's Judged Position

Standing is the card's judged position — status, state,
condition, verdict, health in plainer words. It is called
standing because those words are taken: `status` is the
counters struct and the command, `state` is the store
backend, and `posture` is the aggregate overview. One word,
machine-readable, decided once at assembly — the code both
renderers sign:

| Standing | Console bulb | CLI words |
|---|---|---|
| `ok` | teal | ready / skew / gen (…ago) |
| `hold` | amber | hold lease pinning writes |
| `denied` | red | deny — writes refused |
| `degraded` | amber | degraded — … / skew (degraded) |
| `unreachable` | default border | unreachable — not answering |
| `unproven` | default border | unproven |
| `locked` | default border | locked — writes denied, reads continue |
| `closed` | default border | gateway: closed |
| `unavailable` | default border | unavailable — no … wired |
| `unchecked` | default border | — / unchecked |

The bulb depends only on the standing, never on the kind.
The CLI words depend on the kind: `ok` reads `ready` on the
store, a skew on the clock, `gen (…ago)` on the proof —
each kind voices its own words for the same code.

Assembly rules, tested once in `keeper`:

- edge: deny wins over hold — a refusing fence outranks a
  pinned one;
- store: the lock note promotes from meta to standing —
  `locked` on a locked marker, `degraded` on a marker read
  failure (collapsing the error into unlocked would claim
  intent nobody recorded);
- clock: a local transport is `unchecked`, never `ok` —
  the machine clock is trusted, not probed;
- registry, trust, store: missing evidence never alarms —
  a nil prober reads `unavailable`, a missing generation
  `unproven`, never a guess.

`--json` is the machine view: each card verbatim with its
standing code, plus the same extras the text carries (the
clock stamp beside the card) — never a subset the text
outgrows. Operators read words; scripts read codes.

## Display Kit: Shared Presentation Primitives

Shared formatting lives in `internal/helpers/human`, so both
renderers spell the same values the same way:

- durations and ages (`Dur`, `Ago`, `ShortAge`, `Age`);
- signed clock offsets (`SignedOffset`);
- byte sizes and percents (`Bytes`, `Capacity`,
  `UsedPercent`).
- `keeper.FormatOutcome` prints one activity ring entry for
  either renderer; `keeper.ActivityTail` (10) caps both
  tails.

## Cards: Every Card and Its Two Homes

| Card | `keeper` model | CLI | Console section |
|---|---|---|---|
| Store | `StoreCard` + `Status` counts + proof voice | `status store` | Store |
| Trust | `TrustCard{Standing, Word}` | `trust:` line on `status store` | — (none yet) |
| Registry | `RegistryCard{Standing, URL, Reachable}` | `status registry` | Registry |
| Edge | `EdgeCard{Standing, Open, Deny, Hold, EdgeAddr, RegistryURL}` | `status edge` | Fence |
| Clock | `ClockCard` (raw durations, not words) | `status clock` | Server time |
| Proof | `ProofCard{Standing, Gen, TS}` | `proof:` line on `status store` | Sentinel |
| Activity | `ActivityCard{Outcomes, Total}` | `status activity` | Activity |
| Plan | `Status.Plan` rows | — (`kpr plan` owns the human view) | Plan list |
| Observability | `ObsCard{Links}` | `status observability` | Launchpad |

Console-only, no model: Metrics (`/metrics` serves both),
Configuration (`kpr config`'s job), API Endpoints (static
list).

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
- `EdgeCard.Hold` voices the lease pinning writes now.
- `ClockCard` keeps raw durations; `SkewText`/`NoteText`
  voice them. `Now` stays out of every shared struct —
  each renderer stamps its own render time.

## `kpr status`: The Aggregate, Not a Card

The aggregate, not a card: one posture line per section.
Counters, proof, and ages live behind the cards, not in
it. Without a reachable store it refuses instead of
printing. Here is what it shows:

- registry — reachability, with the probed endpoint beside
  it
- store — state words, with the backend name beside them
- edge — closed, or open with the fence verdict (pass or
  deny)
- clock — live skew with the transport behind it (the state
  names itself when there is no reading)
- plan — how many candidates are due
- activity — none recorded, or how many with the last
  outcome
- observability — unavailable, or how many backends are
  wired

## Where CLI and Console Diverge

The CLI and the console are different ways to read the same
status: terminal lines vs dashboard cards. They stay similar
on purpose, never identical — each voices its medium. What
they owe each other is information parity: every fact
readable in one is readable in the other.

- Standing codes: CLI text voices words (fence effects,
  registry reachability, clock state lines); the console
  maps standing to a bulb class (`bulbClass`: deny red,
  hold/degraded amber, ok teal, else the default border).
- Trust has a CLI line but no console card yet.
- Plan has a console list but no `status plan` twin.
- Proof has no `status proof` command: the card feeds the
  `status store` proof line and the console sentinel
  section.
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

## Adding a Card: Model First, One Slice at a Time

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
