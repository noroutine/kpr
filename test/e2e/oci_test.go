//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// A signed image's signature artifact expires on its own TTL while
// the subject lives on: reap must mark only the artifact row, and the
// sweep must delete the artifact digest while the subject tag stays
// live and fetchable. If this fails, retention eats signed images
// along with their signatures — or orphans signatures forever.
func TestReferrerArtifactSweptSubjectKept(t *testing.T) {
	fx := NewFixture(t)
	s := New(t, fx)

	// The subject: a fresh, untagged-by-TTL image with no row of its
	// own. Nothing about it may become due.
	subject := s.Push("test/signed", "stable", time.Hour)

	// The signature: attached to the subject, tagged for retention,
	// already past its TTL.
	s.AttachArtifact("test/signed", subject, "30s", "application/vnd.kpr.e2e.signature", "kpr-e2e:signature", time.Hour)

	s.ReapArmed()
	s.ExpectDue("test/signed", "30s", "ttl:30s elapsed")
	s.ExpectDueCount(1)

	sum := s.SweepArmed()
	if sum.Performed != 1 || sum.Failed != 0 {
		t.Fatalf("sweep = %+v, want 1 performed, 0 failed", sum)
	}

	s.ExpectAbsentFromCatalog("test/signed", "30s")
	s.ExpectRowGone("test/signed", "30s")

	// The subject survives: tag listed, manifest fetchable.
	s.ExpectTagPresent("test/signed", "stable")
	s.ExpectManifestFetchable("test/signed", subject)
}
