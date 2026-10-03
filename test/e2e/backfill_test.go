//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
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
	list := s.PushUntrackedList("test/multilist", "1s")

	sum := s.BackfillArmed()
	// shadow + list + its two arch children adopted, tracked
	// single and the sentinel floater skipped, nothing failed —
	// the list 400s without an explicit manifest Accept.
	if sum.Recorded != 4 || sum.Skipped != 2 || sum.Failed != 0 {
		t.Fatalf("backfill = %+v, want 4 recorded, 2 skipped, 0 failed", sum)
	}
	listRow := s.ExpectRow("test/multilist", "1s")
	if listRow.Digest != list {
		t.Fatalf("list row digest = %q, want %q", listRow.Digest, list)
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

// The floater is a pointer, not inventory: fossil generations adopt
// with their true (aged) push times, `latest` never becomes a row,
// and a rerun with the live generation tracked proceeds — no
// rollback verdict. Staging proved the failure: unlock stamps the
// gen row with a `now` captured before the mint, so the floater
// link is strictly newer — a `latest` row outranks the served gen
// and every run warns. If this fails, backfill poisons the very
// lineage verdict that gates it.
func TestBackfillSkipsSentinelFloater(t *testing.T) {
	fx := NewStorageFixture(t)
	s := New(t, fx)

	s.PushUntracked(sentinel.Repo, "fossil1")
	s.PushUntracked(sentinel.Repo, "fossil2")
	// Fossils predate kpr: age their links on the bind mount, the
	// way a volume looks after kpr was unwired for a while.
	aged := time.Now().Add(-2 * time.Hour)
	for _, tag := range []string{"fossil1", "fossil2"} {
		link := filepath.Join(fx.StorageDir(), "docker", "registry", "v2",
			"repositories", sentinel.Repo, "_manifests", "tags", tag, "current", "link")
		if err := os.Chtimes(link, aged, aged); err != nil {
			t.Fatalf("age fossil link: %v", err)
		}
	}

	sum := s.BackfillArmed()
	if sum.Recorded != 2 || sum.Skipped != 1 || sum.Failed != 0 {
		t.Fatalf("backfill = %+v, want 2 recorded (fossils), 1 skipped (floater), 0 failed", sum)
	}
	for _, tag := range []string{"fossil1", "fossil2"} {
		fossil := s.ExpectRow(sentinel.Repo, tag)
		if fossil.Actor != backfill.ActorBackfill || fossil.Due {
			t.Fatalf("fossil row %s = %+v, want kpr-backfill + not-due", tag, fossil)
		}
		if time.Since(fossil.PushedAt) < 90*time.Minute {
			t.Fatalf("fossil row %s pushed-at = %v, want the aged link mtime", tag, fossil.PushedAt)
		}
	}
	s.ExpectRowGone(sentinel.Repo, sentinel.Tag)

	// Track the live generation the way unlock does: stamped an
	// hour back, older than the floater link it points at.
	ctx, cancel := s.ctx()
	defer cancel()
	pay, digest, err := sentinel.Read(ctx, s.reg, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served sentinel: %v", err)
	}
	s.RecordRow(sentinel.Repo, pay.Gen, digest, time.Hour)

	// Under the floater-recording code this refuses: the `latest`
	// row wins the newest tiebreak and the served gen reads as a
	// rollback.
	rerun := s.BackfillArmed()
	if rerun.Recorded != 0 || rerun.Skipped != 3 || rerun.Failed != 0 {
		t.Fatalf("rerun = %+v, want 0 recorded, 3 skipped, 0 failed", rerun)
	}
	s.ExpectRowGone(sentinel.Repo, sentinel.Tag)
}
