package keeper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var errTestDown = errors.New("redis down")

// planStage tracks two scratch rows and one app row, none due:
// add must mark by pattern, remove must unmark by pattern.
func planStage() *store.MemStore {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a", PushedAt: keeperNow})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10s", Digest: "sha256:b", PushedAt: keeperNow})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:c", PushedAt: keeperNow})
	return s
}

func dueNames(t *testing.T, s *store.MemStore) []string {
	t.Helper()
	due, err := s.Due(context.Background())
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	var out []string
	for _, r := range due {
		out = append(out, r.Repo+":"+r.Tag)
	}
	return out
}

// Add marks tracked rows matching a Kyverno-style glob with a
// manual reason and leaves the rest alone. If this fails, operators
// cannot hand-pick the plan.
func TestAddPlanMarksMatching(t *testing.T) {
	s := planStage()
	n, err := AddPlan(context.Background(), s, []string{"scratch:*"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if n != 2 {
		t.Errorf("add = %d, want 2 marked", n)
	}
	due, _ := s.Due(context.Background())
	if len(due) != 2 {
		t.Fatalf("due = %v, want the 2 scratch rows", dueNames(t, s))
	}
	for _, r := range due {
		if r.Reason != ManualReason {
			t.Errorf("due reason = %q, want manual", r.Reason)
		}
	}
}

// Adding what matches nothing marks nothing: silence would leave
// the operator guessing whether the pattern worked.
func TestAddPlanNoMatchAddsNothing(t *testing.T) {
	s := planStage()
	n, err := AddPlan(context.Background(), s, []string{"nomatch:*"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if n != 0 {
		t.Errorf("add = %d, want 0", n)
	}
	if due := dueNames(t, s); len(due) != 0 {
		t.Errorf("no-match add marked %v, want nothing", due)
	}
}

// Add with several patterns unions the matches; regex: opts into
// regex like reap --exclude.
func TestAddPlanUnionsPatterns(t *testing.T) {
	s := planStage()
	n, err := AddPlan(context.Background(), s, []string{"scratch:10m", "regex::v1$"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if n != 2 {
		t.Errorf("add = %d, want scratch:10m + app:v1", n)
	}
}

// A bad pattern refuses before anything marks: half a plan from a
// typoed glob is worse than no plan.
func TestAddPlanBadPatternMarksNothing(t *testing.T) {
	s := planStage()
	if _, err := AddPlan(context.Background(), s, []string{"scratch:*", "regex:([ink"}); err == nil {
		t.Fatal("add with invalid regex succeeded, want refusal")
	}
	if due := dueNames(t, s); len(due) != 0 {
		t.Errorf("refused add marked %v, want nothing", due)
	}
}

// Remove drops due marks matching glob/regex/exact and keeps the
// rest due. If this fails, pruning the plan needs a full discard
// plus a fresh reap.
func TestRemovePlanUnmarksMatching(t *testing.T) {
	s := planStage()
	c := context.Background()
	_ = s.MarkDue(c, "scratch", "10m", "ttl:10m elapsed")
	_ = s.MarkDue(c, "app", "v1", "keep-n:exceeds 10")
	n, err := RemovePlan(c, s, []string{"scratch:10m"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if n != 1 {
		t.Errorf("remove = %d, want 1", n)
	}
	if due := dueNames(t, s); len(due) != 1 || due[0] != "app:v1" {
		t.Errorf("due = %v, want only app:v1", due)
	}
}

// Removing what is not due is a no-op, not an error: remove composes
// with reaps that may not have marked the row.
func TestRemovePlanNoMatchNoOp(t *testing.T) {
	s := planStage()
	n, err := RemovePlan(context.Background(), s, []string{"scratch:*"})
	if err != nil {
		t.Fatalf("remove on empty plan: %v", err)
	}
	if n != 0 {
		t.Errorf("remove = %d, want 0", n)
	}
}

// An exact name that matches no tracked row refuses, naming it:
// a typoed image must not slip into (or past) the plan. Globs stay
// lenient (see TestAddPlanNoMatchAddsNothing) — only exact spellings
// are typo-proof. Nothing marks unless everything validates.
func TestAddPlanExactMissRefuses(t *testing.T) {
	for _, patterns := range [][]string{{"ghost:v1"}, {"scratch:10m", "ghost:v1"}} {
		s := planStage()
		_, err := AddPlan(context.Background(), s, patterns)
		if err == nil {
			t.Fatalf("add %v succeeded, want refusal", patterns)
		}
		if got := err.Error(); !strings.Contains(got, "ghost:v1") {
			t.Errorf("refusal %q does not name ghost:v1", got)
		}
		if due := dueNames(t, s); len(due) != 0 {
			t.Errorf("refused add marked %v, want nothing", due)
		}
	}
}

// Discard clears every mark and reports the count; rows survive. If
// this fails, the operator cannot empty the plan without rescanning.
func TestDiscardPlanClearsMarks(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: keeperNow.Add(-time.Hour)})
	_ = s.MarkDue(c, "scratch", "10m", "ttl:10m elapsed")
	_ = s.MarkDue(c, "scratch", "v9", "keep-n:exceeds 10")
	n, err := DiscardPlan(c, s)
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if n != 2 {
		t.Errorf("discard = %d, want 2", n)
	}
	if due := dueNames(t, s); len(due) != 0 {
		t.Errorf("%d marks survived discard, want 0", len(due))
	}
}

// Discarding an empty plan reports zero, not an error.
func TestDiscardPlanEmptyNoOp(t *testing.T) {
	n, err := DiscardPlan(context.Background(), store.NewMemStore())
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if n != 0 {
		t.Errorf("discard = %d, want 0", n)
	}
}

// List returns due rows repo-major with reasons: the plan output
// order contract, pinned at the use case instead of only through
// the CLI's rendering. Tag order opposes repo order on purpose: a
// comparator that falls through to tags must still sort by repo.
func TestListPlanSortsRepoMajor(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "b-repo", Tag: "a", Digest: "sha256:1", PushedAt: keeperNow})
	_ = s.Record(c, policy.Row{Repo: "a-repo", Tag: "z", Digest: "sha256:2", PushedAt: keeperNow})
	_ = s.MarkDue(c, "b-repo", "a", "manual")
	_ = s.MarkDue(c, "a-repo", "z", "manual")
	due, err := ListPlan(c, s)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(due) != 2 || due[0].Repo != "a-repo" || due[1].Repo != "b-repo" {
		t.Errorf("plan = %v, want repo-major order", dueNames(t, s))
	}
	if due[0].Reason != "manual" {
		t.Errorf("reason = %q, want manual", due[0].Reason)
	}
}

// clearFailStore loses the mark table mid-discard: redis down after
// the read, the write failing.
type clearFailStore struct {
	*store.MemStore
}

func (clearFailStore) ClearDue(context.Context) (int, error) {
	return 0, errTestDown
}

// dueFailStore loses the mark table on read: listing against dead
// state must fail, never print an empty plan.
type dueFailStore struct {
	*store.MemStore
}

func (dueFailStore) Due(context.Context) ([]policy.Row, error) {
	return nil, errTestDown
}

// A dead store fails the discard instead of reporting zero: zero
// must mean empty, never unreadable.
func TestDiscardPlanOnDeadStoreFails(t *testing.T) {
	if _, err := DiscardPlan(context.Background(), &clearFailStore{store.NewMemStore()}); err == nil {
		t.Error("discard on dead store succeeded, want an error")
	}
}

// Listing against dead state fails naming redis instead of printing
// an empty plan: "nothing due" must mean empty, never unreadable.
func TestListPlanOnDeadStoreFails(t *testing.T) {
	if _, err := ListPlan(context.Background(), &dueFailStore{store.NewMemStore()}); err == nil {
		t.Error("list on dead store succeeded, want an error")
	}
}
