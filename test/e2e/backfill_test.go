//go:build e2e

package e2e

import (
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/backfill"
)

// A tag pushed past kpr (on disk, no row) must be adopted with its
// digest, link mtime, and backfill actor — while the receiver-known
// tag and our own sentinels stay untouched. If this fails, the
// import door for pre-kpr tags is shut.
func TestBackfillAdoptsPreKprTag(t *testing.T) {
	fx := NewStorageFixture(t)
	s := New(t, fx)

	s.Push("test/tracked", "1s", time.Hour)
	shadow := s.PushUntracked("test/shadow", "1s")

	sum := s.BackfillArmed()
	if sum.Recorded != 1 || sum.Skipped != 1 || sum.Failed != 0 {
		t.Fatalf("backfill = %+v, want 1 recorded, 1 skipped, 0 failed", sum)
	}

	s.ExpectNotDue("test/shadow", "1s")
	row := s.ExpectRow("test/shadow", "1s")
	if row.Digest != shadow {
		t.Fatalf("row digest = %q, want %q", row.Digest, shadow)
	}
	if row.Actor != backfill.ActorBackfill {
		t.Fatalf("row actor = %q, want %q", row.Actor, backfill.ActorBackfill)
	}
	if row.PushedAt.IsZero() || time.Since(row.PushedAt) > 5*time.Minute {
		t.Fatalf("row pushed-at = %v, want the link mtime (~now)", row.PushedAt)
	}
}
