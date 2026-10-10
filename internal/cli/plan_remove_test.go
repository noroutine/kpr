package cli

import (
	"bytes"
	"strings"
	"testing"
)

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
