package cli

import (
	"bytes"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// editStage tracks two scratch rows and one app row, none due:
// plan add must mark by pattern, plan remove must unmark by
// pattern, reap add must take exact images only.
func editStage() *store.MemStore {
	s := store.NewMemStore()
	c := cliCtx()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a", PushedAt: cliNow})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10s", Digest: "sha256:b", PushedAt: cliNow})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:c", PushedAt: cliNow})
	return s
}

// plan add marks tracked rows matching a Kyverno-style glob with a
// manual reason and leaves the rest alone. If this fails, operators
// cannot hand-pick the plan.
func TestPlanAddMarksMatching(t *testing.T) {
	s := editStage()
	var out bytes.Buffer
	if err := runPlanAdd(cliCtx(), &out, s, []string{"scratch:*"}); err != nil {
		t.Fatalf("plan add: %v", err)
	}
	due, _ := s.Due(cliCtx())
	if len(due) != 2 {
		t.Fatalf("due = %v, want the 2 scratch rows", dueTags(due))
	}
	if !strings.Contains(out.String(), "marked 2 rows") {
		t.Errorf("add reported %q, want the count", out.String())
	}
	for _, r := range due {
		if r.Reason != "manual" {
			t.Errorf("due reason = %q, want manual", r.Reason)
		}
	}
}

// Adding what matches nothing says so and marks nothing: silence
// would leave the operator guessing whether the pattern worked.
func TestPlanAddNoMatch(t *testing.T) {
	s := editStage()
	var out bytes.Buffer
	if err := runPlanAdd(cliCtx(), &out, s, []string{"nomatch:*"}); err != nil {
		t.Fatalf("plan add: %v", err)
	}
	if !strings.Contains(out.String(), "no tracked rows matched") {
		t.Errorf("add reported %q, want no-match", out.String())
	}
	if due, _ := s.Due(cliCtx()); len(due) != 0 {
		t.Errorf("no-match add marked %v, want nothing", dueTags(due))
	}
}

// plan add with several patterns unions the matches; regex: opts
// into regex like reap --exclude.
func TestPlanAddUnionsPatterns(t *testing.T) {
	s := editStage()
	var out bytes.Buffer
	if err := runPlanAdd(cliCtx(), &out, s, []string{"scratch:10m", "regex::v1$"}); err != nil {
		t.Fatalf("plan add: %v", err)
	}
	if due, _ := s.Due(cliCtx()); len(due) != 2 {
		t.Errorf("due = %v, want scratch:10m + app:v1", dueTags(due))
	}
}

// A bad pattern refuses before anything marks: half a plan from a
// typoed glob is worse than no plan.
func TestPlanAddBadPatternMarksNothing(t *testing.T) {
	s := editStage()
	var out bytes.Buffer
	if err := runPlanAdd(cliCtx(), &out, s, []string{"scratch:*", "regex:([ink"}); err == nil {
		t.Fatal("plan add with invalid regex succeeded, want refusal")
	}
	if due, _ := s.Due(cliCtx()); len(due) != 0 {
		t.Errorf("refused add marked %v, want nothing", dueTags(due))
	}
}

// plan remove drops due marks matching glob/regex/exact and keeps
// the rest due. If this fails, pruning the plan needs a full
// discard plus a fresh reap.
func TestPlanRemoveUnmarksMatching(t *testing.T) {
	s := editStage()
	c := cliCtx()
	_ = s.MarkDue(c, "scratch", "10m", "ttl:10m elapsed")
	_ = s.MarkDue(c, "app", "v1", "keep-n:exceeds 10")
	var out bytes.Buffer
	if err := runPlanRemove(cliCtx(), &out, s, []string{"scratch:10m"}); err != nil {
		t.Fatalf("plan remove: %v", err)
	}
	due, _ := s.Due(cliCtx())
	if len(due) != 1 || due[0].Repo != "app" {
		t.Errorf("due = %v, want only app:v1", dueTags(due))
	}
	if !strings.Contains(out.String(), "removed 1 due marks") {
		t.Errorf("remove reported %q, want the count", out.String())
	}
}

// Removing what is not due is a no-op, not an error: remove composes
// with reaps that may not have marked the row.
func TestPlanRemoveNoMatchNoOp(t *testing.T) {
	s := editStage()
	var out bytes.Buffer
	if err := runPlanRemove(cliCtx(), &out, s, []string{"scratch:*"}); err != nil {
		t.Fatalf("plan remove on empty plan: %v", err)
	}
}

// reap add takes exact tracked images into the plan. Untracked names
// refuse (MarkDue would conjure phantom rows) and wildcards refuse
// (that spelling is plan add). Nothing marks unless everything
// validates.
func TestReapAddExact(t *testing.T) {
	s := editStage()
	var out bytes.Buffer
	if err := runReapAdd(cliCtx(), &out, s, []string{"scratch:10m", "app:v1"}); err != nil {
		t.Fatalf("reap add: %v", err)
	}
	if due, _ := s.Due(cliCtx()); len(due) != 2 {
		t.Errorf("due = %v, want 2 added rows", dueTags(due))
	}
}

func TestReapAddRefusesUnknownAndWildcards(t *testing.T) {
	for _, images := range [][]string{{"ghost:v1"}, {"scratch:*"}, {"notag"}} {
		s := editStage()
		var out bytes.Buffer
		err := runReapAdd(cliCtx(), &out, s, images)
		if err == nil {
			t.Fatalf("reap add %v succeeded, want refusal", images)
		}
		if !strings.Contains(err.Error(), images[0]) {
			t.Errorf("refusal %q does not name %q", err, images[0])
		}
		if due, _ := s.Due(cliCtx()); len(due) != 0 {
			t.Errorf("refused add marked %v, want nothing", dueTags(due))
		}
	}
}
