package policy_test

import (
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/policy"
)

// The policy package mirrors the sentinel prefix locally (importing
// backfill would cycle: backfill reads policy rows). This drives the
// ghost branch with backfill's own canonical prefix: if it ever stops
// matching, machinery rows fall under the collector. If this fails,
// either the prefix drifted or ghost reaping touches machinery.
func TestGhostSparesCanonicalSentinelPrefix(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	rows := []policy.Row{{
		Repo:     backfill.SentinelPrefix + "deadbeef",
		Tag:      "v1",
		Digest:   "sha256:abc",
		PushedAt: now.Add(-200 * 24 * time.Hour),
	}}
	if got := policy.SelectUntagged(rows, map[string][]string{}, map[string]bool{}, now); len(got) != 0 {
		t.Errorf("selected %v, want none (machinery never reaps)", got)
	}
}
