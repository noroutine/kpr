package keeper_test

import (
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// One formatter voices ring entries everywhere: rows as
// repo:tag — outcome, row-less control events as the outcome
// alone, each with a human age (precise stamps stay in --json).
// The CLI pads for the terminal, the card has its own padding —
// one flag, never two implementations. If this fails, the two
// renderers read different rings.
func TestFormatOutcomeUnifiesBothRenderers(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	then := now.Add(-90 * time.Minute)
	row := keeper.ActivityEntry{Repo: "app", Tag: "v1", Reason: "ttl:10m elapsed", Outcome: "deleted", At: then}
	flip := keeper.ActivityEntry{Outcome: "deny_engage", Reason: "store lock", At: then}
	zero := keeper.ActivityEntry{Repo: "app", Tag: "v1", Reason: "ttl", Outcome: "deleted"}
	for _, tc := range []struct {
		name   string
		entry  keeper.ActivityEntry
		padded bool
		want   string
	}{
		{"cli row", row, true, "  app:v1 — deleted (ttl:10m elapsed), 1h30m ago"},
		{"card row", row, false, "app:v1 — deleted (ttl:10m elapsed), 1h30m ago"},
		{"cli flip", flip, true, "  deny_engage (store lock), 1h30m ago"},
		{"card flip", flip, false, "deny_engage (store lock), 1h30m ago"},
		{"zero stamp", zero, true, "  app:v1 — deleted (ttl), unknown age"},
	} {
		if got := keeper.FormatOutcome(now, tc.entry, tc.padded); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}
