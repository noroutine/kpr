package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// Discarding against dead state fails naming redis: zero must mean
// empty, never unreadable. If this fails, an outage discards on
// paper.
func TestDiscardPlanOnOutageFails(t *testing.T) {
	if err := runDiscardPlan(cliCtx(), io.Discard, deadStore{}); err == nil {
		t.Error("discard on outage succeeded, want an error")
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
