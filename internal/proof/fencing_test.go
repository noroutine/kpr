package proof

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// Cache absent mints; cached refuses naming the endpoint and the
// override; cached with acceptance mints. If this fails, online
// gc collects against a vouched cache believing it reclaimed.
func TestProveBlobCacheOff(t *testing.T) {
	if _, err := ProveBlobCacheOff("", nil); err != nil {
		t.Errorf("absent cache refused: %v", err)
	}
	if _, err := ProveBlobCacheOff("redis:6379", nil); err == nil {
		t.Error("cached registry cleared without acceptance, want refusal")
	} else if got := err.Error(); !strings.Contains(got, "redis:6379") || !strings.Contains(got, "--accept-blob-cache") {
		t.Errorf("refusal = %q, want endpoint and override", got)
	}
	if _, err := ProveBlobCacheOff("redis:6379", Force(Arm(true, false), true)); err != nil {
		t.Errorf("accepted cache refused: %v", err)
	}
	if _, err := ProveBlobCacheOff("redis:6379", Force(Arm(false, false), true)); err == nil {
		t.Error("disarmed acceptance cleared, want refusal (risk without intent is meaningless)")
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
