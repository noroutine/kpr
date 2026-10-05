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

**First migration is already shipped half-done:** `BlobCacheOff`
judges a caller-parsed address string, so it sees the `redis:`
connection half but never the `storage.cache.blobdescriptor`
selection half — `registryRedis` decides, the proof rubber-stamps.
The owned shape is `ProveBlobCacheOff(configPath, accept)`: the
prover reads the config itself, mints when no stanza selects
`redis`, refuses when one does, and the `redis:` block drops to
connection detail for the message. Bonus: a `redis:` block with
no cache stanza stops refusing.

## Provers read their own sources

Same drift, stated generally: a prover that takes a pre-parsed
verdict (`cacheAddr string`, a bare root, a bool) can be told
anything — the evidence-gathering lives in the caller, outside
the seal. The rule going forward: minting functions take the
source (config path, mount, clock) and parse it themselves, so
the judgment the proof embodies is the judgment the proof
performs. `FilesystemStore.Analyze` is the precedent (the stage
reads `Root()` off the token); `ProveBlobCacheOff` is the
exhibit for what happens without it. Apply when touching those
signatures — not now.

## Proofs as binding call dependencies (not revised yet)

The intent was never "call a prove function, then walk a string":
a proof should bind the call — the stage takes the token and
reads what it needs off it, so an unevidenced call does not
compile. `FilesystemStore` was built for that (`Analyze` takes
the token and reads `Root()` itself), but `gc.Run`, `Unlock`,
and backfill still prove-then-unpack into a bare root string,
and the guarantee stops at the call site. The mechanism exists;
the call shapes don't use it yet. Revise when touching those
signatures — not now.
