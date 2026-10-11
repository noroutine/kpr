package keeper_test

import (
	"context"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stageRing returns a mem store with one due row and one ring
// entry: the plan/activity cards' fixture without a backend.
func stageRing(t *testing.T) *store.MemStore {
	t.Helper()
	ctx := context.Background()
	s := store.NewMemStore()
	if err := s.Record(ctx, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:b",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.PushActivity(ctx, store.Outcome{Repo: "app", Tag: "v1",
		Reason: "ttl:10m elapsed", Outcome: "deleted", At: time.Now().UTC()}); err != nil {
		t.Fatalf("push: %v", err)
	}
	return s
}

// The activity card voices the ring with its honest total: the
// tail cap reads "last n of total", never a silent truncation.
// A down store reads unreachable. If this fails, the tail lies
// about the ring.
func TestActivityCardVoicesRingTotal(t *testing.T) {
	ctx := context.Background()
	got := keeper.FetchActivityCard(ctx, stageRing(t))
	if got.Standing != keeper.StandingOK || got.Total != 1 || len(got.Outcomes) != 1 {
		t.Fatalf("activity = (%q, %d/%d), want (ok, 1/1)", got.Standing, len(got.Outcomes), got.Total)
	}
	if o := got.Outcomes[0]; o.Outcome != "deleted" || o.Repo != "app" {
		t.Errorf("entry = %+v, want the pushed outcome voiced", o)
	}
	if got := keeper.FetchActivityCard(ctx, nil); got.Standing != keeper.StandingUnreachable || got.Total != 0 {
		t.Errorf("storeless activity = (%q, %d), want (unreachable, 0)", got.Standing, got.Total)
	}
}
