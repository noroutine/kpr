package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stubCollector replaces the command seam with a shell script: the
// evented runner is exercised without a registry anywhere near.
func stubCollector(script string) func(context.Context, string, []string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ []string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

func collectEvents() (*[]GCEvent, GCReporter) {
	var events []GCEvent
	return &events, func(e GCEvent) { events = append(events, e) }
}

func stages(events []GCEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Stage)
	}
	return out
}

// The runner streams merged stdout/stderr lines to the writer and
// reports the lifecycle start→spawn→started→collect_begin→
// collect_exit. If this fails, gc runs blind: no feedback until the
// exit code.
func TestCollectorStreamsLinesAndReportsStages(t *testing.T) {
	old := collectorCommand
	collectorCommand = stubCollector("echo out-line; echo err-line >&2")
	defer func() { collectorCommand = old }()

	events, report := collectEvents()
	var out strings.Builder
	if err := runCollector(context.Background(), &out, "/bin/sh", nil, report); err != nil {
		t.Fatalf("runCollector: %v", err)
	}
	for _, want := range []string{"out-line", "err-line"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("collector output lacks %q:\n%s", want, out.String())
		}
	}
	wantStages := []string{GCStageStart, GCStageSpawn, GCStageStarted, GCStageCollectBegin, GCStageCollectExit}
	if got := stages(*events); !equalStages(got, wantStages) {
		t.Errorf("stages = %v, want %v", got, wantStages)
	}
}

func equalStages(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A failing collector surfaces the exit plus its last line, and the
// failure event carries both — never a bare "exit status 3". If this
// fails, operators get a number with no clue what the collector
// choked on.
func TestCollectorFailureCarriesLastLine(t *testing.T) {
	old := collectorCommand
	collectorCommand = stubCollector("echo first; echo 'blob eligible for deletion: boom' >&2; exit 3")
	defer func() { collectorCommand = old }()

	events, report := collectEvents()
	var out strings.Builder
	err := runCollector(context.Background(), &out, "/bin/sh", nil, report)
	if err == nil {
		t.Fatal("failing collector returned nil, want the exit surfaced")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error lacks the last line: %v", err)
	}
	got := stages(*events)
	if len(got) == 0 || got[len(got)-1] != GCStageFailure {
		t.Errorf("last stage = %v, want %q", got, GCStageFailure)
	}
}

// Cancelling mid-collect kills the child and reports stopped (never
// a hang, never a leaked process): the cancel fires once the runner
// reports the child started.
func TestCollectorCancelStops(t *testing.T) {
	old := collectorCommand
	collectorCommand = stubCollector("echo ready; sleep 30")
	defer func() { collectorCommand = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, inner := collectEvents()
	report := func(e GCEvent) {
		inner(e)
		if e.Stage == GCStageStarted {
			cancel()
		}
	}
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- runCollector(ctx, &out, "/bin/sh", nil, report) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled collect returned nil, want the cancellation")
		}
		got := stages(*events)
		if len(got) == 0 || got[len(got)-1] != GCStageStopped {
			t.Errorf("last stage = %v, want %q", got, GCStageStopped)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled collect hung, want a prompt stop")
	}
}

// --dry-run reaches the stock binary (the default); --delete-untagged
// composes with it. If this fails, dry-run never asked the collector
// for a preview.
func TestGCArgsDryRun(t *testing.T) {
	dry := gcArgs("/etc/distribution/config.yml", false, true)
	if !hasArg(dry, "--dry-run") {
		t.Errorf("dry gc args = %v, want --dry-run", dry)
	}
	real := gcArgs("/etc/distribution/config.yml", true, false)
	if hasArg(real, "--dry-run") {
		t.Errorf("real gc args = %v, want no --dry-run", real)
	}
	if !hasArg(real, "--delete-untagged") {
		t.Errorf("real gc args = %v, want --delete-untagged kept", real)
	}
}

// The event renderer voices each stage loud: probe verdicts, the
// collector pid (a long mark phase must look alive), the flip
// WARNING, failures with cause. If this fails, gc runs quiet about
// exactly the moments the operator watches.
func TestRenderGCEventVoicesStages(t *testing.T) {
	var out strings.Builder
	report := renderGCEvent(&out, true)
	report(GCEvent{Stage: GCStagePreProbe, Message: "readonly"})
	report(GCEvent{Stage: GCStageStarted, PID: 4242})
	report(GCEvent{Stage: GCStagePostProbe, Message: "readonly"})
	report(GCEvent{Stage: GCStageModeFlip, Message: "readonly→writable"})
	report(GCEvent{Stage: GCStageFailure, Error: "exit status 3: boom"})
	for _, want := range []string{
		"sentinel: registry is READONLY",
		"collector started (pid 4242)",
		"dry-run, nothing will be deleted",
		"sentinel: registry still READONLY",
		"WARNING: registry flipped readonly→writable mid-run",
		"collector failed: exit status 3: boom",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rendered events lack %q:\n%s", want, out.String())
		}
	}
	var real strings.Builder
	renderGCEvent(&real, false)(GCEvent{Stage: GCStageStarted, PID: 7})
	if strings.Contains(real.String(), "dry-run") {
		t.Errorf("real-run start claims dry-run:\n%s", real.String())
	}
	renderGCEvent(&real, false)(GCEvent{Stage: GCStageStarted})
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// A dry-run preview on a writable registry proceeds warned instead
// of refusing: previews delete nothing, so demanding readonly first
// would gate a harmless look behind downtime. The warning must say
// nothing will be deleted. If this fails, previews need the
// readonly flip.
func TestGCDryRunDowngradesWritableRefusal(t *testing.T) {
	accept := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/kpr-gc-probe/blobs/uploads/uuid")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer accept.Close()

	root := t.TempDir()
	cfg := stageGCStore(t, root)
	stageUploadDir(t, root, "uuid")
	s := store.NewMemStore()

	oldBin := registryBinPath
	registryBinPath = stageBin(t, "echo preview-marker")
	defer func() { registryBinPath = oldBin }()

	var out strings.Builder
	if err := runGC(context.Background(), &out, s, accept.URL, cfg, GCOptions{DryRun: true}); err != nil {
		t.Fatalf("dry-run gc on writable = %v, want nil", err)
	}
	for _, want := range []string{"preview only", "nothing will be deleted", "preview-marker"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out.String())
		}
	}
}

// A mode flip mid-run is loud but never a panic: without --force the
// run fails with the WARNING on record; with --force the operator
// presumed to know and the run passes warned. If this fails, flips
// either crash or slip by silent.
func TestGCFlipIsLoudNotPanic(t *testing.T) {
	flap := httptest.NewServer(flipFlop(405, 202))
	defer flap.Close()
	flap2 := httptest.NewServer(flipFlop(405, 202))
	defer flap2.Close()

	staged := func(t *testing.T) (string, *store.MemStore) {
		root := t.TempDir()
		cfg := stageGCStore(t, root)
		digest := "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd"
		s := store.NewMemStore()
		_ = s.Record(context.Background(), policy.Row{Repo: "app", Tag: "v1", Digest: digest, PushedAt: time.Now()})
		stageTagLink(t, root, "app", "v1", digest)
		return cfg, s
	}
	oldBin := registryBinPath
	registryBinPath = stageBin(t, "exit 0")
	defer func() { registryBinPath = oldBin }()

	cfg, s := staged(t)
	var out strings.Builder
	if err := runGC(context.Background(), &out, s, flap.URL, cfg, GCOptions{}); err == nil {
		t.Fatal("gc across a flip succeeded without --force, want failure")
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("unforced flip warned nothing:\n%s", out.String())
	}

	cfg2, s2 := staged(t)
	out.Reset()
	if err := runGC(context.Background(), &out, s2, flap2.URL, cfg2, GCOptions{Force: true}); err != nil {
		t.Fatalf("forced gc across a flip = %v, want nil (operator knows)", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("forced flip warned nothing:\n%s", out.String())
	}
}
