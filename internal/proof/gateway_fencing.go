package proof

import (
	"context"
	"fmt"
	"net"
	"time"
)

// GatewayFencingAvailable clears collection re: the proxy fence: a proven
// edge is listening, so the HOLD lease the collect engages
// actually pins pushes. An unfenced collect against a serving
// registry lets a push land mid-collect — the corruption online
// gc exists to prevent. Sealed like every evidence; nil never
// clears.
//
// The second path to a proof is acceptance: --accept-unfenced on an
// armed run presumes the operator accepts collecting without the
// fence (quiesced writers, or similar). Said at the gate, loudly.
type GatewayFencingAvailable interface {
	sealed()
}

type gatewayFencing struct{}

func (gatewayFencing) sealed() {}

// NOTE(mutants): timeout arithmetic is equivalent — no test
// distinguishes a 2s dial from a 3s one, and none should.
const edgeDialTimeout = 2 * time.Second

// ProveGatewayFencingAvailable proves a gateway *could* fence
// the way serve opens one (RelativeURLs over the registry
// config) plus liveness (the edge addr answers), with the lease
// half first: no shared file store means no HOLD lease dir, so
// there is nothing for the edge to watch even if it listens.
// This is capability, not posture — the lock marker and the
// lease file are never read here. Beside it travels the
// acceptance the flag produced. Any half failing with nothing
// accepted refuses naming which half and the override;
// anything else produces.
func ProveGatewayFencingAvailable(ctx context.Context, configPath, edgeAddr string, leaseReady bool, accept AcceptedRisk) (GatewayFencingAvailable, error) {
	if !leaseReady {
		if accept != nil {
			return gatewayFencing{}, nil
		}
		return nil, fmt.Errorf("proxy HOLD fence unavailable without a shared file store (redis backend keeps no lease dir) — run on the file backend, or re-run with --accept-unfenced")
	}
	if _, err := ProveRelativeURLs(configPath); err != nil {
		if accept != nil {
			return gatewayFencing{}, nil
		}
		return nil, fmt.Errorf("gateway unproven: %v — serve the registry behind a proven edge, or re-run with --accept-unfenced", err)
	}
	dial, cancel := context.WithTimeout(ctx, edgeDialTimeout)
	defer cancel()
	if err := dialEdge(dial, edgeAddr); err != nil {
		if accept != nil {
			return gatewayFencing{}, nil
		}
		return nil, fmt.Errorf("gateway silent on %s: boot serve with the edge on (it fences pushes during collect), or re-run with --accept-unfenced", edgeAddr)
	}
	return gatewayFencing{}, nil
}

// dialEdge is the liveness half, factored for test: a proven
// config with nobody listening is a closed edge, not a fence.
func dialEdge(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}
