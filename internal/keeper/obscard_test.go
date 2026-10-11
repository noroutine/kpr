package keeper_test

import (
	"testing"

	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// The observability card voices the configured UI backends in
// launchpad order, skipping every backend without a URL. Empty
// reads unavailable, never an empty list — the CLI voices that
// standing as "no backends configured". If this fails, the CLI
// and the console link different backends.
func TestObsCardVoicesConfiguredBackends(t *testing.T) {
	got := keeper.FetchObsCard(keeper.ObsURLs{
		Quickwit: "http://qw:8080", Grafana: "http://gf:3000",
	})
	if got.Standing != keeper.StandingOK {
		t.Fatalf("standing = %q, want ok", got.Standing)
	}
	if len(got.Links) != 2 {
		t.Fatalf("links = %d, want 2 (unconfigured backends skipped)", len(got.Links))
	}
	if got.Links[0].Name != "Quickwit" || got.Links[0].URL != "http://qw:8080" || got.Links[0].Blurb == "" {
		t.Errorf("first = %+v, want the Quickwit entry with its blurb", got.Links[0])
	}
	if got.Links[1].Name != "Grafana" {
		t.Errorf("second = %+v, want Grafana (launchpad order kept)", got.Links[1])
	}
	if got := keeper.FetchObsCard(keeper.ObsURLs{}); got.Standing != keeper.StandingUnavailable || len(got.Links) != 0 {
		t.Errorf("empty = (%q, %d), want (unavailable, 0)", got.Standing, len(got.Links))
	}
}
