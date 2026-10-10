package proof

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// A proven config with a listener produces; either half failing
// refuses naming which and the override; acceptance produces either
// way. If this fails, gc believes an unfenced registry is fenced.
func TestProveGatewayFencingAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	proven := writeRegistryConfig(t, "http:\n  addr: :5000\n  relativeurls: true\n")

	if _, err := ProveGatewayFencingAvailable(ctx, proven, ln.Addr().String(), true, nil); err != nil {
		t.Errorf("proven listening edge refused: %v", err)
	}
	if _, err := ProveGatewayFencingAvailable(ctx, proven, ln.Addr().String(), false, nil); err == nil {
		t.Error("leaseless edge cleared, want refusal (nothing to watch)")
	} else if got := err.Error(); !strings.Contains(got, "file store") || !strings.Contains(got, "--accept-unfenced") {
		t.Errorf("refusal = %q, want lease reason and override", got)
	}
	if _, err := ProveGatewayFencingAvailable(ctx, proven, "127.0.0.1:1", true, nil); err == nil {
		t.Error("silent edge cleared, want refusal")
	} else if got := err.Error(); !strings.Contains(got, "127.0.0.1:1") || !strings.Contains(got, "--accept-unfenced") {
		t.Errorf("refusal = %q, want addr and override", got)
	}
	if _, err := ProveGatewayFencingAvailable(ctx, "/nonexistent.yml", ln.Addr().String(), true, nil); err == nil {
		t.Error("unproven config cleared, want refusal")
	}
	accept := Force(Arm(true, false), true)
	if _, err := ProveGatewayFencingAvailable(ctx, proven, "127.0.0.1:1", true, accept); err != nil {
		t.Errorf("accepted silent edge refused: %v", err)
	}
	if _, err := ProveGatewayFencingAvailable(ctx, "/nonexistent.yml", ln.Addr().String(), true, accept); err != nil {
		t.Errorf("accepted unproven config refused: %v", err)
	}
	if _, err := ProveGatewayFencingAvailable(ctx, proven, ln.Addr().String(), false, accept); err != nil {
		t.Errorf("accepted leaseless edge refused: %v", err)
	}
}
