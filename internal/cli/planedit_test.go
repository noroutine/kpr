package cli

import (
	"bytes"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// reportStage tracks one scratch row: just enough to render a count
// against. Behavior lives in keeper; here only the messages.
func reportStage() *store.MemStore {
	s := store.NewMemStore()
	_ = s.Record(cliCtx(), policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a", PushedAt: cliNow})
	return s
}

// plan add reports the marked count with the manual reason stamped
// on. If this fails, the operator sees a count with no provenance.
func TestPlanAddReportsCount(t *testing.T) {
	s := reportStage()
	var out bytes.Buffer
	if err := runPlanAdd(cliCtx(), &out, s, []string{"scratch:*"}); err != nil {
		t.Fatalf("plan add: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "marked 1 rows due (manual)") {
		t.Errorf("add reported %q, want the count with reason", got)
	}
}

// plan remove reports the removed count. If this fails, pruning
// prints a wrong number while the marks went elsewhere.
func TestPlanRemoveReportsCount(t *testing.T) {
	s := reportStage()
	_ = s.MarkDue(cliCtx(), "scratch", "10m", "ttl:10m elapsed")
	var out bytes.Buffer
	if err := runPlanRemove(cliCtx(), &out, s, []string{"scratch:*"}); err != nil {
		t.Fatalf("plan remove: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "removed 1 due marks") {
		t.Errorf("remove reported %q, want the count", got)
	}
}

// Adding what matches nothing says so plainly instead of reporting
// zero marked: silence would leave the operator guessing whether the
// pattern worked.
func TestPlanAddNoMatchReportsEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := runPlanAdd(cliCtx(), &out, reportStage(), []string{"nomatch:*"}); err != nil {
		t.Fatalf("plan add: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "no tracked rows matched") {
		t.Errorf("add reported %q, want no-match", got)
	}
}

// Removing from an empty plan says so plainly instead of reporting
// zero removed: remove composes with reaps that may not have marked.
func TestPlanRemoveEmptyReportsNothingRemoved(t *testing.T) {
	var out bytes.Buffer
	if err := runPlanRemove(cliCtx(), &out, reportStage(), []string{"scratch:*"}); err != nil {
		t.Fatalf("plan remove: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "nothing removed") {
		t.Errorf("remove reported %q, want nothing-removed", got)
	}
}

// discard reports what went, or plainly that nothing was due. If
// this fails, empty and cleared read the same — or different.
func TestPlanDiscardReportsCount(t *testing.T) {
	s := reportStage()
	_ = s.MarkDue(cliCtx(), "scratch", "10m", "ttl:10m elapsed")
	var out bytes.Buffer
	if err := runDiscardPlan(cliCtx(), &out, s); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "discarded 1 due marks") {
		t.Errorf("discard reported %q, want the count", got)
	}
	out.Reset()
	if err := runDiscardPlan(cliCtx(), &out, store.NewMemStore()); err != nil {
		t.Fatalf("empty discard: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "nothing due") {
		t.Errorf("empty discard reported %q, want nothing-due", got)
	}
}
