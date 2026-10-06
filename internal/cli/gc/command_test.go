package gc

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/event"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Driving the command to a dead registry refuses from inside
// the run: every RunE line ahead of the call executes (deps,
// arming, backend, fence, the run call itself). If this fails,
// the command's own wiring runs uncovered in this binary.
func TestCmdReachesRunBeforeAnyGate(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	t.Setenv(config.EnvRegistryURL, "http://127.0.0.1:1")
	var buf bytes.Buffer
	Cmd.SetOut(&buf)
	defer Cmd.SetOut(nil)
	Cmd.SetContext(context.Background())
	if err := Cmd.RunE(Cmd, nil); err == nil {
		t.Fatal("command against a dead registry succeeded, want refusal from inside the run")
	}
}

// Each --accept-* flag mints exactly its own acceptance, nothing
// else — there is no umbrella flag. If this fails, a shared mint
// lets one flag clear another risk's gate.
func TestGcAcceptsMintsPerRisk(t *testing.T) {
	armed := proof.Arm(true, false)
	cases := []struct {
		flag string
		hold func(gcrun.Accepts) proof.AcceptedRisk
	}{
		{"accept-blob-cache", func(a gcrun.Accepts) proof.AcceptedRisk { return a.Cache }},
		{"accept-unfenced", func(a gcrun.Accepts) proof.AcceptedRisk { return a.Fence }},
		{"accept-clock-skew", func(a gcrun.Accepts) proof.AcceptedRisk { return a.ClockSkew }},
		{"accept-rollback", func(a gcrun.Accepts) proof.AcceptedRisk { return a.Rollback }},
		{"accept-mode-flip", func(a gcrun.Accepts) proof.AcceptedRisk { return a.ModeFlip }},
	}
	for _, tc := range cases {
		if err := Cmd.Flags().Set(tc.flag, "true"); err != nil {
			t.Fatalf("set --%s: %v", tc.flag, err)
		}
		got := gcAccepts(armed)
		if tc.hold(got) == nil {
			t.Errorf("--%s minted nothing, want its own acceptance", tc.flag)
		}
		minted := 0
		for _, a := range []proof.AcceptedRisk{got.Cache, got.Fence, got.ClockSkew, got.Rollback, got.ModeFlip} {
			if a != nil {
				minted++
			}
		}
		if minted != 1 {
			t.Errorf("--%s minted %d acceptances, want exactly 1", tc.flag, minted)
		}
		if err := Cmd.Flags().Set(tc.flag, "false"); err != nil {
			t.Fatalf("reset --%s: %v", tc.flag, err)
		}
	}
	// Disarmed mints nothing even with every flag set: acceptance
	// without intent is meaningless, and the mint says so.
	for _, tc := range cases {
		if err := Cmd.Flags().Set(tc.flag, "true"); err != nil {
			t.Fatalf("set --%s: %v", tc.flag, err)
		}
		defer func() { _ = Cmd.Flags().Set(tc.flag, "false") }()
	}
	if got := gcAccepts(proof.Arm(false, false)); got != (gcrun.Accepts{}) {
		t.Errorf("disarmed mint = %+v, want zero", got)
	}
}

// Dry-run is the absence of the mint: flag or env arms, silence
// previews. If this fails, gc collects on nothing or previews when
// armed.
func TestGcDryRunFollowsTheMint(t *testing.T) {
	if !GcDryRun(proof.Arm(false, false)) {
		t.Error("disarmed gc not dry-run, want preview")
	}
	if GcDryRun(proof.Arm(true, false)) {
		t.Error("flag-armed gc dry-run, want collect")
	}
	if GcDryRun(proof.Arm(false, true)) {
		t.Error("env-armed gc dry-run, want collect")
	}
}

// The event renderer voices each stage loud: probe verdicts, the
// collector pid (a long mark phase must look alive), the flip
// WARNING, failures with cause. If this fails, gc runs quiet about
// exactly the moments the operator watches.
func TestRenderGCEventVoicesStages(t *testing.T) {
	var out strings.Builder
	report := renderGCEvent(&out, true)
	report(event.Event{Stage: gcrun.StagePreProbe, Message: "readonly"})
	report(event.Event{Stage: gcrun.StageStarted, PID: 4242})
	report(event.Event{Stage: gcrun.StagePostProbe, Message: "readonly"})
	report(event.Event{Stage: gcrun.StageModeFlip, Message: "readonly→writable"})
	report(event.Event{Stage: gcrun.StageFailure, Error: "exit status 3: boom"})
	report(event.Event{Stage: gcrun.StageHoldEngage})
	report(event.Event{Stage: gcrun.StageHoldRelease})
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
	renderGCEvent(&real, false)(event.Event{Stage: gcrun.StageStarted, PID: 7})
	if strings.Contains(real.String(), "dry-run") {
		t.Errorf("real-run start claims dry-run:\n%s", real.String())
	}
	renderGCEvent(&real, false)(event.Event{Stage: gcrun.StageStarted})
}

// The fence constructor moved to gc with the decision (see
// internal/gc/fence_test.go); the adapter factory here is
// covered where it is built, ring-only: ephemeral narration
// belongs to the caller. If this fails, cli grew a second
// fencing decision beside the use case's.
func TestNewFenceControlRecordsToRing(t *testing.T) {
	st := store.NewMemStore()
	ctl := newFenceControl(st)(t.TempDir())
	release, err := ctl.Hold(context.Background(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("adapter Hold: %v", err)
	}
	release()
	activity, err := st.Activity(context.Background())
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if len(activity) != 2 {
		t.Fatalf("ring holds %d outcomes, want both hold transitions", len(activity))
	}
}
