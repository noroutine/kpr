package gc

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
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

func collectEvents() (*[]Event, Reporter) {
	var events []Event
	return &events, func(e Event) { events = append(events, e) }
}

func stages(events []Event) []string {
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
	report := func(e Event) {
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

// The writable-armed collect demands acceptance at the delete
// boundary: without the token it refuses before the collector
// port is even called. If this fails, the variant stopped
// deciding.
func TestCollectWritableArmedRefusesWithoutRisk(t *testing.T) {
	called := false
	collect := func(context.Context, io.Writer, string, []string, Reporter) error {
		called = true
		return nil
	}
	var out strings.Builder
	err := collectWritableArmed(context.Background(), &out, collect,
		"/bin/sh", []string{"garbage-collect"}, func(Event) {}, nil)
	if err == nil {
		t.Fatal("writable collect without risk succeeded, want the blind refusal")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("err = %q, want the remedy named", err.Error())
	}
	if called {
		t.Error("refused collect reached the collector port")
	}
}

// With the token the variant is a straight delegation: same args,
// same port, nothing added and nothing hidden. If this fails, the
// variant edits the run it only gates.
func TestCollectWritableArmedDelegatesWithRisk(t *testing.T) {
	var got [][]string
	collect := func(_ context.Context, _ io.Writer, _ string, args []string, _ Reporter) error {
		got = append(got, args)
		return nil
	}
	var out strings.Builder
	args := []string{"garbage-collect", "/etc/distribution/config.yml"}
	if err := collectWritableArmed(context.Background(), &out, collect,
		"/bin/sh", args, func(Event) {}, forcedRisk()); err != nil {
		t.Fatalf("writable collect with risk: %v", err)
	}
	if len(got) != 1 || strings.Join(got[0], " ") != strings.Join(args, " ") {
		t.Errorf("delegated args = %v, want %v untouched", got, args)
	}
}
