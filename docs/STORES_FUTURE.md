# Stores future

## Lease convergence

All persisted state belongs to `store`; role ports stay narrow (W16:
one field per splittable role). Single **package**, not single
interface — a fat `Store` would let fence code reach rows.

End-state: `fence` owns a lease port next to `Controller`/`GateStore`,
backends implement it, `FenceForBackend` decides by capability,
`edge` goes fs-free. Staging: `store/hold.go` moved as-is (file
backend semantics frozen); per-backend impls split when a consumer
arrives.

## Per-backend lease honesty

HOLD is cross-process (gc writes, serve reads). That rules
implementations in or out on physics:

| Backend | Verdict | Medium |
|---|---|---|
| file | honest today | lease file in its own dir |
| redis | possible, behavior change | `SET NX EX`; missing key = absent = fail-open; goes from loud-unfenced to fenced — needs a consumer decision, docs, tests |
| mem | impossible as-is | in-process map is invisible to the other process |

Shared-mem idea: a shared segment converges to file semantics with
extra steps — the honest options are file-in-store-dir or a redis
key. Until then mem keeps today's answer: nil fence plus the loud
warning. A fake in-process lease would be theater: gc believing it
is fenced while serve never sees it.

## Constraints for the split

- Fail-open reads preserved per backend: missing and corrupt read
  as absent; expiry is the real bound; release is best-effort.
- The overrun voice (present-but-past) has no redis equivalent —
  TTL auto-deletes — so conformance tests pin file semantics and
  redis semantics separately, not one shared assertion.
- `holdFileName` is a cross-process contract (both processes plus
  operators), hence exported; the file encoding stays unexported.
