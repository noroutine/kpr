package proof

import (
	"fmt"
)

// BlobCacheOff clears collection re: the registry's blobdescriptor
// cache: the config carries none, so deleted blobs stop being
// vouched the moment the collector drops them. With a redis cache
// configured, deletes stay vouched until restart — an online
// collect would lie about what it reclaimed. Sealed like every
// evidence; nil never clears.
//
// The second minting path is acceptance, not evidence: --accept-
// blob-cache on an armed run presumes the operator knows the
// cache is on (offline follow-up restart planned, or similar).
// Which path minted is said at the gate, loudly, in its own
// message — the token carries only that collection is cleared
// re: the cache.
type BlobCacheOff interface {
	sealed()
}

type blobCacheOff struct{}

func (blobCacheOff) sealed() {}

// ProveBlobCacheOff reads the cache endpoint the collector would
// use (empty means no redis cache — inmemory needs no gate) with
// the acceptance the flag minted beside it. Cached with nothing
// accepted refuses with the remedy and the override; anything
// else mints.
func ProveBlobCacheOff(cacheAddr string, accept AcceptedRisk) (BlobCacheOff, error) {
	if cacheAddr == "" {
		return blobCacheOff{}, nil
	}
	if accept != nil {
		return blobCacheOff{}, nil
	}
	return nil, fmt.Errorf("blobdescriptor cache at %s: remove the top-level redis: block from the registry config (online deletes stay vouched until restart), or re-run with --accept-blob-cache", cacheAddr)
}
