# Sentinels — future work

Unbuilt sentinel designs. Shipped behavior lives in
[SENTINELS.md](SENTINELS.md).

## Backfill snapshot detection

Backfill reads the sentinel via the API and compares
generation and timestamp against expectations, so a shared store
holding an old snapshot becomes visible instead of silently
trusted.

Ground already laid: `Verify` refuses typed —
`sentinel.Mismatch` (answered, wrong generation) is distinct from a
plain read error (no evidence). Noted in
[BACKFILL.md](BACKFILL.md).

Payload schema and staleness policy get decided here, not earlier.
