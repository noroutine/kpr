package keeper_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// stubSource answers a pinned skew: the clock card's transport
// matrix without a network.
type stubSource struct {
	offset time.Duration
	err    error
}

func (s stubSource) Offset(context.Context, string) (time.Duration, error) {
	return s.offset, s.err
}

// The clock card voices the transport and the live skew as kpr
// sees it: local trusts the machine clock (unchecked, never a
// fake zero), a dead source reads unreachable (never slow — the
// caller bounds the probe), a skew past tolerance degrades. If
// this fails, the card fakes certainty about time.
func TestClockCardVoicesTransportAndSkew(t *testing.T) {
	ctx := context.Background()
	tol := 30 * time.Second
	for _, tc := range []struct {
		name     string
		method   clock.Method
		src      clock.Source
		standing keeper.Standing
		skew     time.Duration
	}{
		{"local", clock.MethodLocal, stubSource{}, keeper.StandingUnchecked, 0},
		{"dead", clock.MethodHTTPS, stubSource{err: errors.New("no route")}, keeper.StandingUnreachable, 0},
		{"ahead ok", clock.MethodHTTPS, stubSource{offset: 83 * time.Millisecond}, keeper.StandingOK, 83 * time.Millisecond},
		{"behind ok", clock.MethodHTTPS, stubSource{offset: -time.Second}, keeper.StandingOK, -time.Second},
		{"past tolerance", clock.MethodHTTPS, stubSource{offset: 61 * time.Second}, keeper.StandingDegraded, 61 * time.Second},
		{"at tolerance", clock.MethodHTTPS, stubSource{offset: 30 * time.Second}, keeper.StandingOK, 30 * time.Second},
		{"at negative tolerance", clock.MethodHTTPS, stubSource{offset: -30 * time.Second}, keeper.StandingOK, -30 * time.Second},
	} {
		got := keeper.FetchClockCard(ctx, tc.src, tc.method, "ts", tol)
		if got.Standing != tc.standing || got.Skew != tc.skew {
			t.Errorf("%s = (%q, %v), want (%q, %v)",
				tc.name, got.Standing, got.Skew, tc.standing, tc.skew)
		}
		if got.Method != string(tc.method) || got.Tolerance != tol {
			t.Errorf("%s method/tolerance = (%q, %v), want voiced", tc.name, got.Method, got.Tolerance)
		}
	}
	if got := keeper.FetchClockCard(ctx, stubSource{}, clock.MethodLocal, "", tol); got.Server != "machine clock" {
		t.Errorf("local server = %q, want machine clock", got.Server)
	}
}

// A real https source voices a signed skew through the card:
// the card reads the same transport the mints check through.
// If this fails, the clock card cannot read what the proofs
// rely on.
func TestClockCardReadsLiveSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	got := keeper.FetchClockCard(context.Background(), clock.NewSource(clock.MethodHTTPS), clock.MethodHTTPS, srv.URL, 30*time.Second)
	if got.Standing != keeper.StandingOK {
		t.Fatalf("live source = %q, want ok", got.Standing)
	}
	if !strings.HasPrefix(got.SkewText(), "+") && !strings.HasPrefix(got.SkewText(), "-") {
		t.Errorf("skew text = %q, want a signed offset", got.SkewText())
	}
	if got.NoteText() != "tolerance 30s" {
		t.Errorf("note = %q, want the 30s tolerance named", got.NoteText())
	}
}
