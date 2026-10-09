package gc

import (
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/event"
)

// The default renderer voices each stage loud: probe verdicts, the
// collector pid (a long mark phase must look alive), the flip
// WARNING, failures with cause. If this fails, gc runs quiet about
// exactly the moments the operator watches.
func TestRenderEventVoicesStages(t *testing.T) {
	var out strings.Builder
	report := RenderEvent(&out, true)
	report(event.Event{Stage: StagePreProbe, Message: "readonly"})
	report(event.Event{Stage: StageStarted, PID: 4242})
	report(event.Event{Stage: StagePostProbe, Message: "readonly"})
	report(event.Event{Stage: StageModeFlip, Message: "readonly→writable"})
	report(event.Event{Stage: StageFailure, Error: "exit status 3: boom"})
	report(event.Event{Stage: StageHoldEngage})
	report(event.Event{Stage: StageHoldRelease})
	for _, want := range []string{
		"sentinel: registry is READONLY",
		"collector started (pid 4242)",
		"dry-run, nothing will be deleted",
		"sentinel: registry still READONLY",
		"WARNING: registry flipped readonly→writable mid-run",
		"collector failed: exit status 3: boom",
		"HOLD engaged: manifest writes wait out the armed collect",
		"HOLD released: manifest writes flow again",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rendered events lack %q:\n%s", want, out.String())
		}
	}
	var real strings.Builder
	RenderEvent(&real, false)(event.Event{Stage: StageStarted, PID: 7})
	if strings.Contains(real.String(), "dry-run") {
		t.Errorf("real-run start claims dry-run:\n%s", real.String())
	}
	RenderEvent(&real, false)(event.Event{Stage: StageStarted})
}
