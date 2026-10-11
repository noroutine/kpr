package keeper_test

import (
	"context"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stubGate is a fence with pinned posture: the gateway card's
// snapshot source without a serve loop.
type stubGate struct{ deny, held bool }

func (g stubGate) Snapshot() (bool, bool) { return g.deny, g.held }

// The gateway card voices the live fence posture: open when an
// edge serves, deny on the lock (would-deny, not just
// last-denied), held on the lease, closed with no edge. Deny
// wins over held — the refusal is the news. If this fails, the
// dashboard and the CLI read different fences.
func TestGatewayCardVoicesPosture(t *testing.T) {
	ctx := context.Background()
	open := store.NewMemStore()
	if err := open.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	locked := store.NewMemStore()
	for _, tc := range []struct {
		name  string
		gate  keeper.GatePosture
		st    store.Store
		deny  bool
		held  bool
		stand keeper.Standing
	}{
		{"no edge", nil, open, false, false, keeper.StandingClosed},
		{"open pass", stubGate{}, open, false, false, keeper.StandingOK},
		{"held", stubGate{held: true}, open, false, true, keeper.StandingHeld},
		{"deny", stubGate{deny: true}, nil, true, false, keeper.StandingDenied},
		{"deny wins", stubGate{deny: true, held: true}, nil, true, true, keeper.StandingDenied},
		{"locked denies", stubGate{}, locked, true, false, keeper.StandingDenied},
	} {
		got := keeper.FetchGatewayCard(ctx, tc.gate, tc.st, ":5000", "http://registry:5000")
		if got.Deny != tc.deny || got.Held != tc.held || got.Standing != tc.stand {
			t.Errorf("%s = (deny %v, held %v, %q), want (deny %v, held %v, %q)",
				tc.name, got.Deny, got.Held, got.Standing, tc.deny, tc.held, tc.stand)
		}
		if tc.gate != nil && (!got.Open || got.EdgeAddr != ":5000" || got.RegistryURL != "http://registry:5000") {
			t.Errorf("%s open/addrs = (%v, %q, %q), want edge voiced", tc.name, got.Open, got.EdgeAddr, got.RegistryURL)
		}
	}
}
