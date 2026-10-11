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

// The plan card voices due rows with reasons: the read side of
// plan add/rm, same rows the console lists. A down store reads
// unreachable, never an empty plan. If this fails, the plan
// views read different backlogs.
func TestPlanCardVoicesDueRows(t *testing.T) {
	ctx := context.Background()
	got := keeper.FetchPlanCard(ctx, stageRing(t), nil)
	if got.Standing != keeper.StandingOK || len(got.Rows) != 1 {
		t.Fatalf("plan = (%q, %d rows), want (ok, 1)", got.Standing, len(got.Rows))
	}
	if r := got.Rows[0]; r.Repo != "app" || r.Tag != "v1" || r.Reason != "ttl:10m elapsed" {
		t.Errorf("row = %+v, want the due row voiced", r)
	}
	if got := keeper.FetchPlanCard(ctx, nil, nil); got.Standing != keeper.StandingUnreachable || len(got.Rows) != 0 {
		t.Errorf("storeless plan = (%q, %d rows), want (unreachable, empty)", got.Standing, len(got.Rows))
	}
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
