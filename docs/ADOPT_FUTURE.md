# Adopt — future work

Unbuilt adopt extensions. Shipped behavior lives in
[ADOPT.md](ADOPT.md).

## Contents

- [Adopt-before-unlock window](#adopt-before-unlock-window)

## Adopt-before-unlock window

Unlocking an empty store needs `adopt` first, and between the two
the trust word reads `behind` — served generation untracked —
even though an empty store has nothing behind anything.

Two smells share one root: adopt blesses a generation (baseline)
it never stamped, so the very next verdict compares the store
against a generation the store never wrote.

Options, none taken:

- Adopt stamps the served generation row itself (adopt-record),
  so post-adopt is `paired` and the Heal path stays a backstop
  for wiped-then-repaired stores rather than the normal path.
- Unlock subsumes adopt on empty stores (pair inline as part of
  the mint), so the window never exists.
- The trust word stays silent on zero rows (paired-and-empty is
  a beginning, not a lag).

Whichever lands, `behind` must never read as adopt having
failed.
