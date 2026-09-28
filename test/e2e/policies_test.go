//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// A CI commit build (hex stem + -ttl suffix) whose TTL elapsed must be
// reaped due and swept like a bare TTL tag, while a human-named tag
// with the same suffix shape stays untouched. If this fails, the suffix
// form either never fires end to end or eats names it must not.
func TestCommitHashSuffixSwept(t *testing.T) {
	fx := NewFixture(t)
	s := New(t, fx)

	// Pushed an hour ago: the commit tag expired on arrival, the human
	// tag never expires — the scenario never sleeps on a clock.
	s.Push("test/ci", "abc1234-30s", time.Hour)
	s.Push("test/ci", "myapp-30s", time.Hour)

	s.ReapArmed()
	s.ExpectDue("test/ci", "abc1234-30s", "ttl:30s elapsed")
	s.ExpectNotDue("test/ci", "myapp-30s")
	s.ExpectDueCount(1)

	sum := s.SweepArmed()
	if sum.Performed != 1 || sum.Failed != 0 {
		t.Fatalf("sweep = %+v, want 1 performed, 0 failed", sum)
	}

	s.ExpectAbsentFromCatalog("test/ci", "abc1234-30s")
	s.ExpectRowGone("test/ci", "abc1234-30s")
	s.ExpectTagPresent("test/ci", "myapp-30s")
}

// Beyond the freshest KeepN tags, the oldest per repo must be reaped
// with the keep-n reason and swept away while the survivors stay live.
// If this fails, retention silently keeps everything (disk grows) or
// eats fresh tags.
func TestKeepNOldestSwept(t *testing.T) {
	fx := NewFixture(t)
	s := New(t, fx)

	// Twelve real pushes, oldest first: only the two oldest may go.
	for i := 12; i >= 1; i-- {
		s.Push("test/releases", fmt.Sprintf("v%d", i), time.Duration(i)*time.Hour)
	}

	s.ReapArmed()
	s.ExpectDue("test/releases", "v12", "keep-n:exceeds 10")
	s.ExpectDue("test/releases", "v11", "keep-n:exceeds 10")
	for i := 1; i <= 10; i++ {
		s.ExpectNotDue("test/releases", fmt.Sprintf("v%d", i))
	}
	s.ExpectDueCount(2)

	sum := s.SweepArmed()
	if sum.Performed != 2 || sum.Failed != 0 {
		t.Fatalf("sweep = %+v, want 2 performed, 0 failed", sum)
	}

	s.ExpectAbsentFromCatalog("test/releases", "v12")
	s.ExpectAbsentFromCatalog("test/releases", "v11")
	s.ExpectRowGone("test/releases", "v12")
	s.ExpectRowGone("test/releases", "v11")
}

// A digest-less row older than the stale-upload age is push residue,
// not a retry in flight: reap must mark it, and the sweep must fall
// back to the tag (the manifest underneath still exists) and delete
// it. If this fails, interrupted pushes pile up forever.
func TestStaleUploadSweptViaTagFallback(t *testing.T) {
	fx := NewFixture(t)
	s := New(t, fx)

	// A real manifest under the tag, then the digest-less twin row —
	// what a tracked push looks like after its digest is lost.
	s.Push("test/partial", "upload", time.Hour)
	s.RecordRow("test/partial", "upload", "", 25*time.Hour)

	s.ReapArmed()
	s.ExpectDue("test/partial", "upload", "partial:older than 24h")

	sum := s.SweepArmed()
	if sum.Performed != 1 || sum.Failed != 0 {
		t.Fatalf("sweep = %+v, want 1 performed, 0 failed", sum)
	}

	s.ExpectAbsentFromCatalog("test/partial", "upload")
	s.ExpectRowGone("test/partial", "upload")
}

// A tag deleted upstream past the grace period leaves its manifest
// behind: reap must mark the row untagged, and the sweep must resolve
// the already-gone digest as performed, not failed. If this fails,
// external deletions either linger as rows or count as failures.
func TestUntaggedSweptAfterGrace(t *testing.T) {
	fx := NewFixture(t)
	s := New(t, fx)

	old := s.Push("test/legacy", "1.0", 8*24*time.Hour)
	s.Push("test/legacy", "2.0", time.Hour)

	// Upstream deletes the old manifest out from under kpr; the row
	// still carries its digest. The fresh anchor tag keeps the repo
	// listed so the untagged selector sees a fetched catalog.
	s.DeleteManifest("test/legacy", old)

	s.ReapArmed()
	s.ExpectDue("test/legacy", "1.0", "untagged:past grace")
	s.ExpectNotDue("test/legacy", "2.0")

	sum := s.SweepArmed()
	if sum.Performed != 1 || sum.Failed != 0 {
		t.Fatalf("sweep = %+v, want 1 performed, 0 failed", sum)
	}

	s.ExpectAbsentFromCatalog("test/legacy", "1.0")
	s.ExpectRowGone("test/legacy", "1.0")
}
