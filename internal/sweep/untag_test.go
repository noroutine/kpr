package sweep

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

func untagRow(repo, tag, digest string) policy.Row {
	return policy.Row{Repo: repo, Tag: tag, Digest: digest,
		PushedAt: sweepNow.Add(-time.Hour), Actor: "kpr-receiver"}
}

func untagSweeper(s store.Store, stub *stubRegistry) *Sweeper {
	return &Sweeper{Store: s, Registry: stub,
		Now: func() time.Time { return sweepNow },
		Log: func(context.Context, string, ...any) {}}
}

// Untag is the operator-directed delete: named rows, no due marks
// required, deleted by digest through the same port the pass uses.
// If this fails, rm --untag bypasses the use case again.
func TestUntagDeletesByDigestDropsRow(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := untagSweeper(s, stub)
	done, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")})
	if err != nil {
		t.Fatalf("Untag: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("Untag confirmed %d rows, want 1", len(done))
	}
	if len(stub.refs) != 1 || stub.refs[0] != "app@sha256:aaa" {
		t.Errorf("refs = %v, want [app@sha256:aaa] (by digest, not tag)", stub.refs)
	}
	if rows, _ := s.All(testCtx()); len(rows) != 0 {
		t.Errorf("untagged row left behind: %+v", rows)
	}
	acts, _ := s.Activity(testCtx())
	if len(acts) != 1 || acts[0].Outcome != "deleted" {
		t.Errorf("activity = %+v, want one deleted outcome", acts)
	}
	if len(acts) == 1 && acts[0].Actor != "kpr-sweep" {
		t.Errorf("activity actor = %q, want the sweeper named", acts[0].Actor)
	}
	if len(acts) == 1 && acts[0].Trigger != "untag" {
		t.Errorf("activity trigger = %q, want untag (not a pass trigger)", acts[0].Trigger)
	}
}

// Already-gone counts as confirmed: the tag is gone, the row has
// nothing left to track. If this fails, re-untagging a raced
// delete errors instead of converging.
func TestUntagGoneDropsRow(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	stub := &stubRegistry{outcome: registry.OutcomeGone}
	sw := untagSweeper(s, stub)
	if _, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}); err != nil {
		t.Fatalf("Untag on gone tag: %v", err)
	}
	if rows, _ := s.All(testCtx()); len(rows) != 0 {
		t.Errorf("gone row left behind: %+v", rows)
	}
}

// A held delete keeps its row and fails loudly: dropping it would
// untrack a live tag silently. If this fails, refusals bury rows.
func TestUntagHeldKeepsRowLoud(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	stub := &stubRegistry{outcome: registry.OutcomeHeld}
	sw := untagSweeper(s, stub)
	if _, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}); err == nil {
		t.Fatal("held untag succeeded, want a loud failure")
	}
	if rows, _ := s.All(testCtx()); len(rows) != 1 {
		t.Errorf("held untag dropped the row: %d left, want 1", len(rows))
	}
}

// failDeleteStore breaks row drops while the registry confirms:
// the manifest is gone but the row survives.
type failDeleteStore struct {
	store.Store
	n        int
	failFrom int
}

func (s *failDeleteStore) Delete(_ context.Context, repo, tag string) error {
	s.n++
	if s.n >= s.failFrom {
		return errors.New("store down")
	}
	return s.Store.Delete(context.Background(), repo, tag)
}

// A local row-drop failure names the row and continues the batch
// (like the pass loop): the manifest is already gone, a re-untag
// converges on `gone`. If this fails, one sick row abandons the
// rest unattempted and unreported.
func TestUntagRowDropFailureContinues(t *testing.T) {
	s := &failDeleteStore{failFrom: 1, Store: func() store.Store {
		m := store.NewMemStore()
		_ = m.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
		_ = m.Record(testCtx(), untagRow("app", "v2", "sha256:bbb"))
		return m
	}()}
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := untagSweeper(s, stub)
	_, err := sw.Untag(testCtx(), []policy.Row{
		untagRow("app", "v1", "sha256:aaa"),
		untagRow("app", "v2", "sha256:bbb"),
	})
	if err == nil || !strings.Contains(err.Error(), "row drop failed") {
		t.Fatalf("err = %v, want the row-drop failure named", err)
	}
	if len(stub.refs) != 2 {
		t.Errorf("refs = %v, want both rows attempted", stub.refs)
	}
}

// Untrack drops rows without touching the registry: bare `rm`.
// The tag survives untracked; the outcome journals as untracked.
// If this fails, row drops bypass the use case again.
func TestUntrackDropsRowsOnly(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	sw := untagSweeper(s, &stubRegistry{})
	done, err := sw.Untrack(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")})
	if err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("Untrack confirmed %d rows, want 1", len(done))
	}
	if rows, _ := s.All(testCtx()); len(rows) != 0 {
		t.Errorf("untracked row left behind: %+v", rows)
	}
	acts, _ := s.Activity(testCtx())
	if len(acts) != 1 || acts[0].Outcome != "untracked" {
		t.Errorf("activity = %+v, want one untracked outcome", acts)
	}
	if len(acts) == 1 && acts[0].Actor != "kpr-sweep" {
		t.Errorf("activity actor = %q, want the sweeper named", acts[0].Actor)
	}
	if len(acts) == 1 && acts[0].Trigger != "rm" {
		t.Errorf("activity trigger = %q, want rm (bare rm untags nothing)", acts[0].Trigger)
	}
}

// A mid-batch row-drop failure still reports the confirmed rows:
// partial progress prints before the error returns. If this fails,
// the operator never learns what the error left behind.
func TestUntrackPartialReportsDone(t *testing.T) {
	m := store.NewMemStore()
	_ = m.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	_ = m.Record(testCtx(), untagRow("app", "v2", "sha256:bbb"))
	s := &failDeleteStore{failFrom: 2, Store: m}
	sw := untagSweeper(s, &stubRegistry{})
	done, err := sw.Untrack(testCtx(), []policy.Row{
		untagRow("app", "v1", "sha256:aaa"),
		untagRow("app", "v2", "sha256:bbb"),
	})
	if err == nil {
		t.Fatal("partial untrack succeeded, want the failure named")
	}
	if len(done) != 1 {
		t.Fatalf("done = %d rows, want the 1 confirmed row reported", len(done))
	}
}

// One bad row fails the call but the good ones still resolve: the
// error names every failure, confirmed rows are already gone.
func TestUntagPartialFailureNamesAll(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	_ = s.Record(testCtx(), untagRow("app", "v2", "sha256:bbb"))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted, failFirst: true, err: errors.New("denied")}
	sw := untagSweeper(s, stub)
	// Order-independent: failFirst hits whichever ref arrives first.
	_, err := sw.Untag(testCtx(), []policy.Row{
		untagRow("app", "v1", "sha256:aaa"),
		untagRow("app", "v2", "sha256:bbb"),
	})
	if err == nil {
		t.Fatal("partial untag succeeded, want the failure named")
	}
	rows, _ := s.All(testCtx())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the 1 failed row kept", len(rows))
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("error = %q, want the cause named", err.Error())
	}
}
