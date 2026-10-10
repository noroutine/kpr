# Architecture — future work

What is open or coming. [ARCHITECTURE.md](ARCHITECTURE.md)
describes what ships today.

## Contents

- [Token-auth registries](#token-auth-registries)
- [Bind the fs proof to the served endpoint](#bind-the-fs-proof-to-the-served-endpoint)
- [Close the mark-to-delete race](#close-the-mark-to-delete-race)
- [One torn-row rule across backends](#one-torn-row-rule-across-backends)
- [Real partial-upload detection](#real-partial-upload-detection)
- [Presenters out of command bodies](#presenters-out-of-command-bodies)
- [Registry metrics as a GC-readiness signal](#registry-metrics-as-a-gc-readiness-signal)
- [Tag-release flow](#tag-release-flow)
- [keep-N tuning surface](#keep-n-tuning-surface)

## Token-auth registries

kpr is scoped to anonymous and basic registries today. Against a
token-issuing registry, every kpr-owned call — enumeration,
sentinel, proofs — 401s.

The fix is client-side only; kpr never verifies JWT:

1. Read the issuer realm from the 401 `Bearer` challenge.
2. Present the same user+password pair to it.
3. Fetch per-scope tokens (`registry:catalog:*`, per-repo `pull`).
4. Retry with `Bearer`.

One credential form covers both schemes, and the stock collector
binary is unaffected — it takes its own auth from registry config.

**Sharp edge:** the sentinel classifies mode via upload-initiate,
which needs a *push*-scoped token on the probe repo. Without one,
classification must refuse rather than guess. Likewise a loud
refusal when the issuer won't grant catalog scope.

## Bind the fs proof to the served endpoint

The filestore proof establishes that the mount is a registry
store, but nothing binds it to the *served* endpoint — a
generation read off the local root is never checked against the
served one. A stale or foreign mount passes the proof while
serving something else. Open: retiring this unbound-fs-witness
limitation means comparing the locally read generation with the
served generation wherever both are available.

## Close the mark-to-delete race

Between the mark (what's due) and the delete (what goes), a push
can land: the row was correct at mark time, wrong at delete
time. The re-read before deleting narrows the window, and the
harm on losing is a retryable dangling tag — which is why this
stays open, deferred until bitten. Full closure needs either a
readonly fence held across deletes or a push-epoch CAS;
equivalently, the sweep running as a gc phase under such a
fence.

## One torn-row rule across backends

A torn row (half-written JSON) refuses on the file backend and
skips on redis today — two rules for one event. Open: a single
`store.Store` contract case pinning the behavior, so backends
can't drift apart again.

## Real partial-upload detection

The `partial` policy detects interrupted pushes by absence
(digest-less past max age), not by inspection. Real detection
would read the manifest (bounded) and say what's actually
missing. Open.

## Presenters out of command bodies

Rendering stays testable only when it lives outside the
command: pure render functions (like `backfillLines`) that port
tests assert as data, with thin goldens asserting strings.
`resolveBackfillSink` is the first cut; the rest of the command
bodies follow as touched. Open.

## Registry metrics as a GC-readiness signal

Storage pressure could gate collection (collect because the
disk says so, not only because the plan says so). Noted, not
scheduled.

## Tag-release flow

Image push plus Forgejo release as one flow. Unverified —
written down so the idea doesn't evaporate, no design behind it
yet.

## keep-N tuning surface

Declined. N stays 10, with `--exclude` for release lines.
Per-repo tuning (a `--last` or `--include` surface) stays out by
decision: retention policy is global, exclusions name the
exceptions.
