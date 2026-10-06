package proof

import (
	"errors"
	"strings"
	"testing"
)

// The prover owns both halves of the concert: a bare redis:
// block and a storage.cache.blobdescriptor stanza (any backend)
// each refuse, naming what was found and the override; both
// absent mints. If this fails, online gc collects against a
// vouched cache believing it reclaimed.
func TestProveBlobCacheOff(t *testing.T) {
	bare := writeRegistryConfig(t, "storage:\n  filesystem:\n    rootdirectory: /tmp/root\n")
	if _, err := ProveBlobCacheOff(bare, nil); err != nil {
		t.Errorf("cache-free config refused: %v", err)
	}
	redisBlock := writeRegistryConfig(t, "redis:\n  addr: redis:6379\n")
	if _, err := ProveBlobCacheOff(redisBlock, nil); err == nil {
		t.Error("redis block cleared without acceptance, want refusal")
	} else if got := err.Error(); !strings.Contains(got, "redis:6379") || !strings.Contains(got, "--accept-blob-cache") {
		t.Errorf("refusal = %q, want endpoint and override", got)
	}
	stanza := writeRegistryConfig(t, "storage:\n  cache:\n    blobdescriptor: redis\n")
	if _, err := ProveBlobCacheOff(stanza, nil); err == nil {
		t.Error("blobdescriptor stanza cleared without acceptance, want refusal")
	} else if got := err.Error(); !strings.Contains(got, "blobdescriptor") || !strings.Contains(got, "--accept-blob-cache") {
		t.Errorf("refusal = %q, want stanza and override", got)
	}
	inmemory := writeRegistryConfig(t, "storage:\n  cache:\n    blobdescriptor: inmemory\n")
	if _, err := ProveBlobCacheOff(inmemory, nil); err == nil {
		t.Error("inmemory stanza cleared, want refusal (backend is irrelevant, caching is caching)")
	}
	both := writeRegistryConfig(t, "redis:\n  addr: redis:6379\nstorage:\n  cache:\n    blobdescriptor: redis\n")
	if _, err := ProveBlobCacheOff(both, nil); err == nil {
		t.Error("both halves cleared, want refusal")
	} else if got := err.Error(); !strings.Contains(got, "redis:6379") || !strings.Contains(got, "blobdescriptor") {
		t.Errorf("refusal = %q, want both halves named", got)
	}
	// Plural addrs follow the collector's grammar: the first wins.
	// If this fails, the prover dials a different redis than the
	// collector it clears.
	plural := writeRegistryConfig(t, "redis:\n  addr: first:6379\n  addrs:\n  - winner:6379\n  - second:6379\n")
	if _, err := ProveBlobCacheOff(plural, nil); err == nil {
		t.Error("plural redis addrs cleared, want refusal")
	} else if got := err.Error(); !strings.Contains(got, "winner:6379") || strings.Contains(got, "first:6379") {
		t.Errorf("refusal = %q, want the winning addr", got)
	}
	if _, err := ProveBlobCacheOff(redisBlock, Force(Arm(true, false), true)); err != nil {
		t.Errorf("accepted cache refused: %v", err)
	}
	if _, err := ProveBlobCacheOff(redisBlock, Force(Arm(false, false), true)); err == nil {
		t.Error("disarmed acceptance cleared, want refusal (risk without intent is meaningless)")
	}
}

// Broken input is not a missing proof: a missing file and a
// garbage config refuse even beside acceptance — the override
// waives a risk, never an unreadable. Callers route this outside
// the checklist via errors.As. If this fails, acceptance can
// launder a config the prover never saw.
func TestProveBlobCacheOffUnreadable(t *testing.T) {
	accept := Force(Arm(true, false), true)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"missing", "/nonexistent-registry.yml"},
		{"garbage", writeRegistryConfig(t, "redis:\n\tbad: [unclosed")},
	} {
		if _, err := ProveBlobCacheOff(tc.path, nil); err == nil {
			t.Errorf("%s config cleared, want refusal", tc.name)
		} else {
			var unreadable Unreadable
			if !errors.As(err, &unreadable) {
				t.Errorf("%s refusal = %T, want Unreadable for the err branch", tc.name, err)
			}
			// The type voices itself: the message names the path,
			// and the cause unwraps for callers that log it. If
			// this fails, the err branch reports a pathless cause.
			if got := err.Error(); !strings.Contains(got, tc.path) {
				t.Errorf("%s message = %q, want the path", tc.name, got)
			}
			if errors.Unwrap(err) == nil {
				t.Errorf("%s unwraps to nothing, want the cause", tc.name)
			}
		}
		if _, err := ProveBlobCacheOff(tc.path, accept); err == nil {
			t.Errorf("%s config accepted, want refusal (acceptance cannot fix unreadable)", tc.name)
		}
	}
}
