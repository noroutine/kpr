package keeper

import (
	"context"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// GatewayCard is the live fence posture through the same read
// the console renders: open, deny, or held, with the endpoints
// beside it. Disabled renders closed, never an error. Standing
// is decided here, at assembly — deny wins over held, and the
// lock read overrides the snapshot (would-deny, not just
// last-denied).
type GatewayCard struct {
	Standing    Standing `json:"standing"`
	Open        bool     `json:"open"`
	Deny        bool     `json:"deny"`
	Hold        bool     `json:"hold"`
	EdgeAddr    string   `json:"edgeAddr,omitempty"`
	RegistryURL string   `json:"registryUrl,omitempty"`
}

// GatePosture is the outbound port the gateway card reads: the
// fence snapshot. *fence.Gate carries it; tests pin it.
type GatePosture interface {
	Snapshot() (deny, held bool)
}

// FetchGatewayCard voices the fence posture: the snapshot's
// deny/held, overridden by the live lock reading whenever a
// store is present — so a fresh lock reads deny before the
// first refused push and a fresh unlock clears without waiting
// for traffic. An unreadable marker reads deny — the fence
// refuses then too.
func FetchGatewayCard(ctx context.Context, gate GatePosture, s store.Store, edgeAddr, registryURL string) GatewayCard {
	if gate == nil {
		// Closed, but the endpoints are config facts, not probe
		// results — they render either way.
		return GatewayCard{Standing: StandingClosed, EdgeAddr: edgeAddr, RegistryURL: registryURL}
	}
	deny, held := gate.Snapshot()
	if s != nil {
		unlocked, err := s.IsUnlocked(ctx)
		deny = err != nil || !unlocked
	}
	card := GatewayCard{Open: true, Deny: deny, Hold: held, EdgeAddr: edgeAddr, RegistryURL: registryURL}
	switch {
	case deny:
		card.Standing = StandingDenied
	case held:
		card.Standing = StandingHeld
	default:
		card.Standing = StandingOK
	}
	return card
}
