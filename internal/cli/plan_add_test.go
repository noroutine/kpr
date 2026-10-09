package cli

import (
	"bytes"
	"strings"
	"testing"
)

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
