package proof

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
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
		}
		if _, err := ProveBlobCacheOff(tc.path, accept); err == nil {
			t.Errorf("%s config accepted, want refusal (acceptance cannot fix unreadable)", tc.name)
		}
	}
}

// A proven config with a listener mints; either half failing
// refuses naming which and the override; acceptance mints either
// way. If this fails, gc believes an unfenced registry is fenced.
func TestProveGatewayFencing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	proven := writeRegistryConfig(t, "http:\n  addr: :5000\n  relativeurls: true\n")

	if _, err := ProveGatewayFencing(ctx, proven, ln.Addr().String(), true, nil); err != nil {
		t.Errorf("proven listening edge refused: %v", err)
	}
	if _, err := ProveGatewayFencing(ctx, proven, ln.Addr().String(), false, nil); err == nil {
		t.Error("leaseless edge cleared, want refusal (nothing to watch)")
	} else if got := err.Error(); !strings.Contains(got, "file store") || !strings.Contains(got, "--accept-unfenced") {
		t.Errorf("refusal = %q, want lease reason and override", got)
	}
	if _, err := ProveGatewayFencing(ctx, proven, "127.0.0.1:1", true, nil); err == nil {
		t.Error("silent edge cleared, want refusal")
	} else if got := err.Error(); !strings.Contains(got, "127.0.0.1:1") || !strings.Contains(got, "--accept-unfenced") {
		t.Errorf("refusal = %q, want addr and override", got)
	}
	if _, err := ProveGatewayFencing(ctx, "/nonexistent.yml", ln.Addr().String(), true, nil); err == nil {
		t.Error("unproven config cleared, want refusal")
	}
	accept := Force(Arm(true, false), true)
	if _, err := ProveGatewayFencing(ctx, proven, "127.0.0.1:1", true, accept); err != nil {
		t.Errorf("accepted silent edge refused: %v", err)
	}
	if _, err := ProveGatewayFencing(ctx, "/nonexistent.yml", ln.Addr().String(), true, accept); err != nil {
		t.Errorf("accepted unproven config refused: %v", err)
	}
	if _, err := ProveGatewayFencing(ctx, proven, ln.Addr().String(), false, accept); err != nil {
		t.Errorf("accepted leaseless edge refused: %v", err)
	}
}
