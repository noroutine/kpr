# Proofs — future work

Unbuilt proof designs. The shipped proof model lives in
[PROOFS.md](PROOFS.md).

## Proofs over registry config

Some kpr demands are about the registry's own *configuration*
rather than runtime evidence:

- The dev stack's missing `blobdescriptor` cache ([GC.md](GC.md))
  only holds while nobody re-adds the section.
- A `storage.delete.enabled: false` registry 405s every sweep.

Today those are docs and warnings.

**The shape** would follow the existing pattern: a prover reads the
mounted config kpr already resolves for gc, and mints only when the
knob says what kpr needs.

**The wrinkle** is lifetime. Config is read at boot, while proofs
are minted per run — so the evidence would be a boot-time reading
re-checked per use, never a fresh one.

**One knob already works this way:** `http.relativeurls`, sealed
as `RelativeURLs`. It is fence-critical, so it refuses rather than
warns — no proof, no edge. See [GATEWAY.md](GATEWAY.md). It is the
precedent the rest would follow, not future work.

Open: which knobs follow, and where refusing stops being
affordable.
