//go:build e2e

// Package e2e holds the maintained end-to-end scenarios: the keeper
// pipeline (push → reap → sweep) against real containers, driven
// through a small scenario DSL (scenario.go) over testcontainers
// fixtures (fixture.go). Run with:
//
//	go test -tags e2e ./test/e2e/ -count=1
//
// or `make e2e` / `just e2e` (same, with -race). The unit suite never
// builds this package (the e2e tag excludes it), so `go test ./...`
// stays instant and docker-free. Scenarios skip when no docker answers;
// they never invent results.
package e2e

import (
	"testing"
	"time"
)

// A pushed image whose TTL elapsed must be reaped due (reasoned) and
// swept by digest until neither the catalog nor the rows know it. If
// this fails, the MVP pipeline — the one thing kpr exists to do —
// broke between push and delete.
func TestTTLExpirySweep(t *testing.T) {
	fx := NewFixture(t)
	s := New(t, fx)

	// Pushed an hour ago with a 30s TTL: expired on arrival, so the
	// scenario never sleeps on a clock.
	s.Push("test/busybox", "30s", time.Hour)

	s.ReapArmed()
	s.ExpectDue("test/busybox", "30s", "ttl:30s elapsed")

	sum := s.SweepArmed()
	if sum.Performed != 1 || sum.Failed != 0 {
		t.Fatalf("sweep = %+v, want 1 performed, 0 failed", sum)
	}

	s.ExpectAbsentFromCatalog("test/busybox", "30s")
	s.ExpectRowGone("test/busybox", "30s")
}
