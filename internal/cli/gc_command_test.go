package cli

import (
	"bytes"
	"context"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
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
		hold func(gc.Accepts) proof.AcceptedRisk
	}{
		{"accept-blob-cache", func(a gc.Accepts) proof.AcceptedRisk { return a.Cache }},
		{"accept-unfenced", func(a gc.Accepts) proof.AcceptedRisk { return a.Fence }},
		{"accept-clock-skew", func(a gc.Accepts) proof.AcceptedRisk { return a.ClockSkew }},
		{"accept-rollback", func(a gc.Accepts) proof.AcceptedRisk { return a.Rollback }},
		{"accept-mode-flip", func(a gc.Accepts) proof.AcceptedRisk { return a.ModeFlip }},
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
	if got := gcAccepts(proof.Arm(false, false)); got != (gc.Accepts{}) {
		t.Errorf("disarmed mint = %+v, want zero", got)
	}
}
