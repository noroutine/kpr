package keeper_test

import (
	"context"
	"errors"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// stubReg answers the reachability probe without HTTP: the card
// must consume the registry through the Prober port, never a
// concrete client. If this fails, the card is coupled to the
// transport.
type stubReg struct{ err error }

func (s stubReg) Reachable(context.Context) error { return s.err }

// The registry card voices reachability with the probed
// endpoint: reachable names the URL, a down registry still
// names it (unreachable says where, not just what), and no
// prober reads unavailable. If this fails, status registry
// and the console section disagree about what was probed.
func TestRegistryCardVoicesEndpoint(t *testing.T) {
	ctx := context.Background()
	up := keeper.FetchRegistryCard(ctx, stubReg{}, "http://registry:5000")
	if up.Standing != keeper.StandingOK || !up.Reachable || up.URL != "http://registry:5000" {
		t.Errorf("up = (%q, %v, %q), want (ok, true, url)", up.Standing, up.Reachable, up.URL)
	}
	down := keeper.FetchRegistryCard(ctx, stubReg{err: errors.New("connection refused")}, "http://registry:5000")
	if down.Standing != keeper.StandingUnreachable || down.Reachable || down.URL != "http://registry:5000" {
		t.Errorf("down = (%q, %v, %q), want (unreachable, false, url)", down.Standing, down.Reachable, down.URL)
	}
	if got := keeper.FetchRegistryCard(ctx, nil, "http://registry:5000"); got.Standing != keeper.StandingUnavailable || got.Reachable {
		t.Errorf("proberless = (%q, %v), want (unavailable, false)", got.Standing, got.Reachable)
	}
}

// NewRegistryCard reuses an established answer without
// probing: the aggregate posture and the console reuse the
// FetchStatus probe instead of health-checking twice per
// render. If this fails, every render doubles the outbound
// probe and the shared answer goes unread.
func TestNewRegistryCardReusesProbe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reg       keeper.Prober
		reachable bool
		stand     keeper.Standing
	}{
		{"ok stays ok", stubReg{}, true, keeper.StandingOK},
		{"down stays down", stubReg{}, false, keeper.StandingUnreachable},
		{"no prober stays unavailable", nil, false, keeper.StandingUnavailable},
		{"no prober never upgrades", nil, true, keeper.StandingUnavailable},
	} {
		got := keeper.NewRegistryCard(tc.reg, tc.reachable, "http://registry:5000")
		if got.Standing != tc.stand || got.Reachable != (tc.stand == keeper.StandingOK) || got.URL != "http://registry:5000" {
			t.Errorf("%s = (%q, %v), want (%q, answer-kept)", tc.name, got.Standing, got.Reachable, tc.stand)
		}
	}
}
