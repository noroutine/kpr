package proof

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// BlobCacheOff clears collection re: the registry's blobdescriptor
// cache: the config carries none, so deleted blobs stop being
// vouched the moment the collector drops them. With a cache
// configured, deletes stay vouched until restart — an online
// collect would lie about what it reclaimed. Sealed like every
// evidence; nil never clears.
//
// The second path to a proof is acceptance, not evidence: --accept-
// blob-cache on an armed run presumes the operator knows the
// cache is on (offline follow-up restart planned, or similar).
// Which path produced is said at the gate, loudly, in its own
// message — the token carries only that collection is cleared
// re: the cache.
type BlobCacheOff interface {
	sealed()
}

type blobCacheOff struct{}

func (blobCacheOff) sealed() {}

// Unreadable marks a registry config the prover could not read
// or parse: broken input, not a missing proof. Acceptance cannot
// fix unreadable, so even a fully armed override refuses —
// callers route this outside the checklist via errors.As, never
// as a [miss] with a remedy.
type Unreadable struct {
	Path string
	Err  error
}

func (e Unreadable) Error() string {
	return fmt.Sprintf("registry config unreadable at %s: %v", e.Path, e.Err)
}

func (e Unreadable) Unwrap() error { return e.Err }

// ProveBlobCacheOff reads the registry config itself and judges
// both halves of the caching concert: the top-level redis:
// connection block and the storage.cache.blobdescriptor
// selection stanza (any backend — caching is caching). Only
// both absent produce; either present refuses naming what was
// found and the override. Unreadable input refuses as
// Unreadable, waived by nothing.
func ProveBlobCacheOff(configPath string, accept AcceptedRisk) (BlobCacheOff, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, Unreadable{Path: configPath, Err: err}
	}
	var cfg struct {
		Redis struct {
			Addrs []string `yaml:"addrs"`
			Addr  string   `yaml:"addr"`
		} `yaml:"redis"`
		Storage struct {
			Cache struct {
				BlobDescriptor string `yaml:"blobdescriptor"`
			} `yaml:"cache"`
		} `yaml:"storage"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, Unreadable{Path: configPath, Err: err}
	}
	addr := cfg.Redis.Addr
	if len(cfg.Redis.Addrs) > 0 {
		addr = cfg.Redis.Addrs[0]
	}
	stanza := cfg.Storage.Cache.BlobDescriptor
	if addr == "" && stanza == "" {
		return blobCacheOff{}, nil
	}
	if accept != nil {
		return blobCacheOff{}, nil
	}
	switch {
	case addr != "" && stanza != "":
		return nil, fmt.Errorf("blobdescriptor cache at %s with storage.cache.blobdescriptor %q: remove the top-level redis: block and the stanza from the registry config (online deletes stay vouched until restart), or re-run with --accept-blob-cache", addr, stanza)
	case addr != "":
		return nil, fmt.Errorf("blobdescriptor cache at %s: remove the top-level redis: block from the registry config (online deletes stay vouched until restart), or re-run with --accept-blob-cache", addr)
	default:
		return nil, fmt.Errorf("blobdescriptor cache %q (storage.cache.blobdescriptor): remove the stanza from the registry config (online deletes stay vouched until restart), or re-run with --accept-blob-cache", stanza)
	}
}
