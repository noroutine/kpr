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
