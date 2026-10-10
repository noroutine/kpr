package sweep

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

func untagRow(repo, tag, digest string) policy.Row {
	return policy.Row{Repo: repo, Tag: tag, Digest: digest,
		PushedAt: sweepNow.Add(-time.Hour), Actor: "kpr-receiver"}
}

// unlockedProof reads the marker the way the cli does: paired ground
// arrives unlocked, so this only fails when the ground (not Untag)
// is broken.
func unlockedProof(t *testing.T, s store.Store) proof.UnlockedStore {
	t.Helper()
	unlocked, err := proof.ProveUnlockedStore(testCtx(), s)
	if err != nil {
		t.Fatalf("prove unlocked ground: %v", err)
	}
	return unlocked
}

// untagProof produces on paired ground the way the cli does: the
// caller proves, Untag checks. If producing fails here, the test
// ground (not Untag) is broken.
func untagProof(t *testing.T, s store.Store, dryRun bool) proof.SameStore {
	t.Helper()
	same, err := proof.Prover{
		Sentinel: pairGround(s), Store: s,
		DryRun: dryRun, Now: func() time.Time { return sweepNow },
	}.Prove(testCtx())
	if err != nil {
		t.Fatalf("prove on paired ground: %v", err)
	}
	return same
}

// staleProof serves a generation older than tracked: dry-run
// semantics proceed warned, and the token rides Stale. If Untag
// accepts it, deletes land against a moved registry.
func staleProof(t *testing.T, s store.Store) proof.SameStore {
	t.Helper()
	ctx := testCtx()
	if err := s.SetIdentity(ctx, store.Identity{ID: "test-lineage"}); err != nil {
		t.Fatalf("stage identity: %v", err)
	}
	if err := s.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	old := policy.Row{Repo: sentinel.Repo, Tag: "gen-old",
		PushedAt: sweepNow.Add(-2 * time.Hour), Actor: "kpr-gc"}
	new := policy.Row{Repo: sentinel.Repo, Tag: "gen-new",
		PushedAt: sweepNow.Add(-time.Hour), Actor: "kpr-gc"}
	if err := s.Record(ctx, old); err != nil {
		t.Fatalf("stage old generation: %v", err)
	}
	if err := s.Record(ctx, new); err != nil {
		t.Fatalf("stage new generation: %v", err)
	}
	api := stubSentinel{pay: sentinel.Payload{
		V: 1, Gen: "gen-old", ID: "test-lineage",
		TS: sweepNow.Format(time.RFC3339), Writer: "kpr-gc",
	}}
	same, err := proof.Prover{
		Sentinel: api, Store: s,
		DryRun: true, Now: func() time.Time { return sweepNow },
	}.Prove(ctx)
	if err != nil {
		t.Fatalf("prove stale ground: %v", err)
	}
	if !same.Stale() {
		t.Fatal("stale ground produced a fresh token, want Stale")
	}
	return same
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
	done, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}, untagProof(t, s, false), unlockedProof(t, s))
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

// No token, no delete: Untag without proof refuses before the
// first manifest, and the registry sees zero DELETEs. If this
// fails, the signature stopped deciding.
func TestUntagRefusesWithoutProof(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	// Unlocked ground: this test isolates the missing identity
	// token, not the marker (locked refusal has its own test).
	if err := s.SetUnlocked(testCtx(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := untagSweeper(s, stub)
	if _, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}, nil, unlockedProof(t, s)); err == nil {
		t.Fatal("untag without proof succeeded, want the blind refusal")
	}
	if got := stubDeletes(stub); got != 0 {
		t.Errorf("proofless untag attempted %d deletes", got)
	}
	if rows, _ := s.All(testCtx()); len(rows) != 1 {
		t.Errorf("rows = %d, want the 1 row kept", len(rows))
	}
}

// A stale token refuses too: the served view lags tracked state,
// so the delete would land against a moved registry. If this
// fails, preview-semantics tokens arm real deletes.
func TestUntagRefusesStaleProof(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := untagSweeper(s, stub)
	if _, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}, staleProof(t, s), unlockedProof(t, s)); err == nil {
		t.Fatal("untag on stale proof succeeded, want the stale refusal")
	} else if !strings.Contains(err.Error(), "stale") {
		t.Errorf("err = %q, want the stale refusal named", err.Error())
	}
	if got := stubDeletes(stub); got != 0 {
		t.Errorf("stale untag attempted %d deletes", got)
	}
	if rows, _ := s.All(testCtx()); len(rows) != 3 {
		t.Errorf("rows = %d, want all 3 kept (target plus staged generations)", len(rows))
	}
}

// Locked refuses before identity: a locked store deletes nothing,
// even holding a fresh token — intent gates before evidence. If
// this fails, the token order stopped meaning anything.
func TestUntagRefusesLockedStore(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := untagSweeper(s, stub)
	same, err := proof.Prover{
		Sentinel: pairGround(s), Store: s,
		Now: func() time.Time { return sweepNow },
	}.Prove(testCtx())
	if err != nil {
		t.Fatalf("prove on paired ground: %v", err)
	}
	// pairGround unlocks; re-lock to isolate the marker.
	if err := s.SetUnlocked(testCtx(), false); err != nil {
		t.Fatalf("stage lock: %v", err)
	}
	if _, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}, same, nil); err == nil {
		t.Fatal("untag on locked store succeeded, want the locked refusal")
	} else if !strings.Contains(err.Error(), "locked") {
		t.Errorf("err = %q, want the locked refusal named", err.Error())
	}
	if got := stubDeletes(stub); got != 0 {
		t.Errorf("locked untag attempted %d deletes", got)
	}
}

// Bare rm obeys the marker too: forgetting rows is still dropping
// them. If this fails, the freeze has a hole exactly where the
// operator cleans up.
func TestUntrackRefusesLockedStore(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), untagRow("app", "v1", "sha256:aaa"))
	sw := untagSweeper(s, &stubRegistry{})
	if _, err := sw.Untrack(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}, nil); err == nil {
		t.Fatal("untrack on locked store succeeded, want the locked refusal")
	}
	if rows, _ := s.All(testCtx()); len(rows) != 1 {
		t.Errorf("rows = %d, want the 1 row kept", len(rows))
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
	if _, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}, untagProof(t, s, false), unlockedProof(t, s)); err != nil {
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
	if _, err := sw.Untag(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}, untagProof(t, s, false), unlockedProof(t, s)); err == nil {
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
	}, untagProof(t, s, false), unlockedProof(t, s))
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
	if err := s.SetUnlocked(testCtx(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	sw := untagSweeper(s, &stubRegistry{})
	done, err := sw.Untrack(testCtx(), []policy.Row{untagRow("app", "v1", "sha256:aaa")}, unlockedProof(t, s))
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
	if err := m.SetUnlocked(testCtx(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	s := &failDeleteStore{failFrom: 2, Store: m}
	sw := untagSweeper(s, &stubRegistry{})
	done, err := sw.Untrack(testCtx(), []policy.Row{
		untagRow("app", "v1", "sha256:aaa"),
		untagRow("app", "v2", "sha256:bbb"),
	}, unlockedProof(t, s))
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
	}, untagProof(t, s, false), unlockedProof(t, s))
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

// The ring never renders (): a reason-less row resolves with the
// trigger as its cause, a failed one with the error, and a due
// reason always survives. If this fails, the next rm --untag
// prints another empty pair of braces.
func TestUntagOutcomesAlwaysCarryReason(t *testing.T) {
	s := store.NewMemStore()
	row := untagRow("app", "v1", "sha256:aaa")
	_ = s.Record(testCtx(), row)
	sw := untagSweeper(s, &stubRegistry{outcome: registry.OutcomeDeleted})
	if _, err := sw.Untag(testCtx(), []policy.Row{row}, untagProof(t, s, false), unlockedProof(t, s)); err != nil {
		t.Fatalf("Untag: %v", err)
	}
	acts, _ := s.Activity(testCtx())
	if len(acts) != 1 || acts[0].Reason != "untag" {
		t.Fatalf("activity = %+v, want the trigger as reason", acts)
	}

	dueStore := store.NewMemStore()
	due := untagRow("app", "v2", "sha256:bbb")
	due.Due, due.Reason = true, "ttl:10m elapsed"
	_ = dueStore.Record(testCtx(), due)
	swDue := untagSweeper(dueStore, &stubRegistry{outcome: registry.OutcomeDeleted})
	if _, err := swDue.Untag(testCtx(), []policy.Row{due}, untagProof(t, dueStore, false), unlockedProof(t, dueStore)); err != nil {
		t.Fatalf("Untag: %v", err)
	}
	dueActs, _ := dueStore.Activity(testCtx())
	if len(dueActs) != 1 || dueActs[0].Reason != "ttl:10m elapsed" {
		t.Fatalf("activity = %+v, want the due reason kept", dueActs)
	}

	failStore := store.NewMemStore()
	_ = failStore.Record(testCtx(), untagRow("app", "v3", "sha256:ccc"))
	swFail := untagSweeper(failStore, &stubRegistry{failFirst: true, err: errors.New("registry held the delete")})
	if _, err := swFail.Untag(testCtx(), []policy.Row{untagRow("app", "v3", "sha256:ccc")}, untagProof(t, failStore, false), unlockedProof(t, failStore)); err == nil {
		t.Fatal("Untag succeeded, want the stub failure")
	}
	failActs, _ := failStore.Activity(testCtx())
	if len(failActs) != 1 || !strings.Contains(failActs[0].Reason, "registry held the delete") {
		t.Fatalf("activity = %+v, want the error as reason", failActs)
	}
}
