package gc

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// Killing a never-started command is a silent no-op: the cancel path
// calls it on whatever exists. If this fails, a failed spawn panics
// the cleanup.
func TestKillCollectorNilProcess(t *testing.T) {
	killCollector(&exec.Cmd{})
}

// stubCollector replaces the command seam with a shell script: the
// evented runner is exercised without a registry anywhere near.
func stubCollector(script string) func(context.Context, string, []string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ []string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

func collectEvents() (*[]fence.Event, fence.Reporter) {
	var events []fence.Event
	return &events, func(e fence.Event) { events = append(events, e) }
}

func stages(events []fence.Event) []string {
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
	if err := RunCollector(context.Background(), &out, "/bin/sh", nil, report); err != nil {
		t.Fatalf("RunCollector: %v", err)
	}
	for _, want := range []string{"out-line", "err-line"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("collector output lacks %q:\n%s", want, out.String())
		}
	}
	wantStages := []string{StageStart, StageSpawn, StageStarted, StageCollectBegin, StageCollectExit}
	if got := stages(*events); !equalStages(got, wantStages) {
		t.Errorf("stages = %v, want %v", got, wantStages)
	}
	for _, e := range *events {
		if e.Stage == StageStarted && e.PID <= 0 {
			t.Errorf("started event carries pid %d, want the live child", e.PID)
		}
	}
}

// A scanner read error surfaces as the failure's last line, never
// swallowed: the pipe is the only feedback channel. If this fails, a
// broken collector stream reports success with no output.
func TestScanGCOutputSurfacesReadError(t *testing.T) {
	lines := scanGCOutput(errReader{})
	line, ok := <-lines
	if !ok {
		t.Fatal("erroring reader closed the channel without the error")
	}
	if line.err == nil {
		t.Error("read error came back as text, want it in err")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

// A cancelled context refuses before spawning: no child outlives
// the caller that gave up. If this fails, a cancelled gc still
// forks the collector.
func TestCollectorCancelledContextSpawnsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, report := collectEvents()
	var out strings.Builder
	if err := RunCollector(ctx, &out, "/bin/sh", nil, report); err == nil {
		t.Fatal("RunCollector on cancelled context succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A missing binary fails at spawn with the cause attached: the
// operator sees "not found", not a bare exit code. If this fails,
// a bad collector path reports a mystery failure.
func TestCollectorMissingBinaryFailsAtSpawn(t *testing.T) {
	_, report := collectEvents()
	var out strings.Builder
	if err := RunCollector(context.Background(), &out, "/nonexistent-collector", nil, report); err == nil {
		t.Fatal("RunCollector on missing binary succeeded, want failure")
	} else if !strings.Contains(err.Error(), "collector:") {
		t.Errorf("failure names no spawn cause: %v", err)
	}
}

// Nobody listening kills the child and fails: collecting deaf
// helps no one. The child prints first and lingers, so the write
// error lands in the live feed, not the drain. If this fails, a
// broken status pipe runs the collector to completion unheard.
func TestCollectorDeadListenerKillsChild(t *testing.T) {
	old := collectorCommand
	collectorCommand = stubCollector("echo heard-nothing; sleep 5")
	defer func() { collectorCommand = old }()

	_, report := collectEvents()
	if err := RunCollector(context.Background(), errWriter{errors.New("status pipe closed")},
		"/bin/sh", nil, report); err == nil {
		t.Fatal("RunCollector with dead listener succeeded, want failure")
	} else if !strings.Contains(err.Error(), "collector output:") {
		t.Errorf("failure names no output cause: %v", err)
	}
}

// A quiet fast child still walks the drain: the multiplex loop may
// exit on the reaped child with the channel already closed, or
// with lines still pending. If this fails, the exit path skips
// the drain the comments promise.
func TestCollectorQuietChildDrainsClean(t *testing.T) {
	old := collectorCommand
	collectorCommand = stubCollector("echo parting-word")
	defer func() { collectorCommand = old }()

	_, report := collectEvents()
	var out strings.Builder
	if err := RunCollector(context.Background(), &out, "/bin/sh", nil, report); err != nil {
		t.Fatalf("RunCollector: %v", err)
	}
	if !strings.Contains(out.String(), "parting-word") {
		t.Errorf("drained output lacks the line:\n%s", out.String())
	}
}

// The default seam builds the stock binary command untouched: the
// override exists for tests, production runs the real thing. If
// this fails, the default points somewhere other than the binary.
func TestDefaultCollectorCommandTargetsBinary(t *testing.T) {
	cmd := defaultCollectorCommand(context.Background(), "/usr/bin/registry", []string{"garbage-collect", "x.yml"})
	if cmd == nil || cmd.Path == "" {
		t.Fatal("default collector command builds nothing")
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "garbage-collect" {
		t.Errorf("default collector args = %v, want the stock invocation", cmd.Args)
	}
}

// A fast child is reaped while its last lines still sit in the
// pipe: returning on the exit first drops them (CI caught this as
// an empty collect). Five thousand lines with an instant exit must
// arrive whole and in order. If this fails, the runner loses
// collector output under load.
func TestCollectorDrainsEveryLine(t *testing.T) {
	old := collectorCommand
	collectorCommand = stubCollector("i=1; while [ $i -le 5000 ]; do echo line-$i; i=$((i+1)); done")
	defer func() { collectorCommand = old }()

	events, report := collectEvents()
	var out strings.Builder
	if err := RunCollector(context.Background(), &out, "/bin/sh", nil, report); err != nil {
		t.Fatalf("RunCollector: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5000 {
		t.Fatalf("drained %d lines, want all 5000", len(lines))
	}
	if lines[0] != "line-1" || lines[4999] != "line-5000" {
		t.Errorf("drain endpoints = %q..%q, want line-1..line-5000", lines[0], lines[4999])
	}
	got := stages(*events)
	if len(got) == 0 || got[len(got)-1] != StageCollectExit {
		t.Errorf("last stage = %v, want %q", got, StageCollectExit)
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
	err := RunCollector(context.Background(), &out, "/bin/sh", nil, report)
	if err == nil {
		t.Fatal("failing collector returned nil, want the exit surfaced")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error lacks the last line: %v", err)
	}
	got := stages(*events)
	if len(got) == 0 || got[len(got)-1] != StageFailure {
		t.Errorf("last stage = %v, want %q", got, StageFailure)
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
	report := func(e fence.Event) {
		inner(e)
		if e.Stage == StageStarted {
			cancel()
		}
	}
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- RunCollector(ctx, &out, "/bin/sh", nil, report) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled collect returned nil, want the cancellation")
		}
		got := stages(*events)
		if len(got) == 0 || got[len(got)-1] != StageStopped {
			t.Errorf("last stage = %v, want %q", got, StageStopped)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled collect hung, want a prompt stop")
	}
}

// --dry-run reaches the stock binary (the default); --delete-untagged
// composes with it. If this fails, dry-run never asked the collector
// for a preview.
func TestArgsDryRun(t *testing.T) {
	dry := Args("/etc/distribution/config.yml", false, true)
	if !hasArg(dry, "--dry-run") {
		t.Errorf("dry gc args = %v, want --dry-run", dry)
	}
	real := Args("/etc/distribution/config.yml", true, false)
	if hasArg(real, "--dry-run") {
		t.Errorf("real gc args = %v, want no --dry-run", real)
	}
	if !hasArg(real, "--delete-untagged") {
		t.Errorf("real gc args = %v, want --delete-untagged kept", real)
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// The writable-armed collect demands the preflight clearance at
// the delete boundary: without either token it refuses before the
// collector port is even called. If this fails, the variant
// stopped deciding.
func TestCollectWritableArmedRefusesWithoutClearance(t *testing.T) {
	called := false
	collect := func(context.Context, io.Writer, string, []string, fence.Reporter) error {
		called = true
		return nil
	}
	var out strings.Builder
	err := collectWritableArmed(context.Background(), &out, collect,
		"/bin/sh", []string{"garbage-collect"}, func(fence.Event) {}, nil, nil)
	if err == nil {
		t.Fatal("writable collect without clearance succeeded, want the blind refusal")
	} else if !strings.Contains(err.Error(), "--accept-blob-cache") {
		t.Errorf("err = %q, want the cache override named", err.Error())
	}
	if called {
		t.Error("refused collect reached the collector port")
	}
	// Acceptance mints the kind without the world: the boundary
	// cannot tell proven from accepted, and must not need to.
	accept := proof.Force(proof.Arm(true, false), true)
	cache, err := proof.ProveBlobCacheOff(stageOnlineConfig(t, t.TempDir(), true), accept)
	if err != nil {
		t.Fatalf("stage cache clearance: %v", err)
	}
	err = collectWritableArmed(context.Background(), &out, collect,
		"/bin/sh", []string{"garbage-collect"}, func(fence.Event) {}, cache, nil)
	if err == nil {
		t.Fatal("writable collect with half clearance succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "--accept-unfenced") {
		t.Errorf("err = %q, want the fence override named", err.Error())
	}
	if called {
		t.Error("refused collect reached the collector port")
	}
}

// With both tokens the variant is a straight delegation: same
// args, same port, nothing added and nothing hidden. If this
// fails, the variant edits the run it only gates.
func TestCollectWritableArmedDelegatesWhenCleared(t *testing.T) {
	var got [][]string
	collect := func(_ context.Context, _ io.Writer, _ string, args []string, _ fence.Reporter) error {
		got = append(got, args)
		return nil
	}
	var out strings.Builder
	args := []string{"garbage-collect", "/etc/distribution/config.yml"}
	accept := proof.Force(proof.Arm(true, false), true)
	cache, cacheErr := proof.ProveBlobCacheOff(stageOnlineConfig(t, t.TempDir(), true), accept)
	gating, fenceErr := proof.ProveGatewayFencingAvailable(context.Background(), "/nonexistent.yml", "127.0.0.1:1", false, accept)
	if cacheErr != nil || fenceErr != nil {
		t.Fatalf("stage clearance: %v %v", cacheErr, fenceErr)
	}
	if err := collectWritableArmed(context.Background(), &out, collect,
		"/bin/sh", args, func(fence.Event) {}, cache, gating); err != nil {
		t.Fatalf("writable collect when cleared: %v", err)
	}
	if len(got) != 1 || strings.Join(got[0], " ") != strings.Join(args, " ") {
		t.Errorf("delegated args = %v, want %v untouched", got, args)
	}
}

// A fast child that exits while lines still sit in the pipe must
// lose nothing: the exit ends the multiplex, and the post-loop
// drain feeds the remainder. A slow writer holds the multiplex
// back so the remainder exists deterministically — two hundred
// lines at a millisecond each against an instant burst. If this
// fails, trailing collector output silently vanishes whenever the
// exit wins the race.
func TestCollectorBurstDrainsAfterExit(t *testing.T) {
	old := collectorCommand
	collectorCommand = stubCollector("i=1; while [ $i -le 200 ]; do echo burst-$i; i=$((i+1)); done")
	defer func() { collectorCommand = old }()

	events, report := collectEvents()
	var buf strings.Builder
	out := slowWriter{w: &buf, delay: time.Millisecond}
	if err := RunCollector(context.Background(), &out, "/bin/sh", nil, report); err != nil {
		t.Fatalf("RunCollector: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 200 {
		t.Fatalf("drained %d lines, want all 200", len(lines))
	}
	for i, line := range lines {
		if want := "burst-" + strconv.Itoa(i+1); line != want {
			t.Fatalf("line %d = %q, want %q (order broke across the drain)", i, line, want)
		}
	}
	got := stages(*events)
	if len(got) == 0 || got[len(got)-1] != StageCollectExit {
		t.Errorf("last stage = %v, want %q", got, StageCollectExit)
	}
}

// slowWriter holds back a fast writer by a fixed delay per Write:
// the multiplex cannot keep up with an instant child, so the
// post-loop drain has remainder to feed.
type slowWriter struct {
	w     io.Writer
	delay time.Duration
}

func (s slowWriter) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.w.Write(p)
}
