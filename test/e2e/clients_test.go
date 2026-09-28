//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// Every supported push client must produce a sweepable row: the same
// TTL-expiry flow, driven through ggcr, crane, the docker daemon, and
// the regclient library. Registries accept several manifest flavors;
// the sweeper must delete them all by digest. If one row fails, that
// client's serialization needs attention — not the policy.
func TestPushClientsMatrix(t *testing.T) {
	fx := NewFixture(t)

	for _, tc := range []struct {
		name   string
		client PushClient
	}{
		{"ggcr", ClientGGCR},
		{"crane", ClientCrane},
		{"docker", ClientDocker},
		{"regclient", ClientRegclient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(t, fx)
			repo := "test/client-" + tc.name

			// Pushed an hour ago with a 30s TTL: expired on arrival.
			digest := s.PushWithClient(tc.client, repo, "30s", time.Hour)
			if digest == "" {
				t.Fatal("empty digest after push")
			}

			s.ReapArmed()
			s.ExpectDue(repo, "30s", "ttl:30s elapsed")

			sum := s.SweepArmed()
			if sum.Performed != 1 || sum.Failed != 0 {
				t.Fatalf("sweep = %+v, want 1 performed, 0 failed", sum)
			}

			s.ExpectAbsentFromCatalog(repo, "30s")
			s.ExpectRowGone(repo, "30s")
		})
	}
}

// A multi-arch index tagged with a TTL must sweep by its index digest:
// the tag leaves the catalog even though the per-arch children stay
// addressable underneath (blob GC is offline work, not the sweeper's).
// If this fails, platform manifests dodge retention entirely.
func TestMultiarchIndexSwept(t *testing.T) {
	fx := NewFixture(t)
	s := New(t, fx)

	digest := s.PushIndex("test/multi", "24h", 25*time.Hour, "amd64", "arm64")
	if digest == "" {
		t.Fatal("empty index digest after push")
	}

	s.ReapArmed()
	s.ExpectDue("test/multi", "24h", "ttl:24h0m0s elapsed")

	sum := s.SweepArmed()
	if sum.Performed != 1 || sum.Failed != 0 {
		t.Fatalf("sweep = %+v, want 1 performed, 0 failed", sum)
	}

	s.ExpectAbsentFromCatalog("test/multi", "24h")
	s.ExpectRowGone("test/multi", "24h")
}
