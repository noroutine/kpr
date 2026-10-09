package gc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"nrtn.dev/catalyst/kpr/internal/event"
)

// GC lifecycle stages. Probes and proofs report through these alongside
// the collector itself, so one event stream tells the whole run: what
// the sentinel saw before, what the collector said, what it saw after.
const (
	stageStart        = "start"
	stageSpawn        = "spawn"
	stageStarted      = "started"
	stagePreProbe     = "pre_probe"
	stageCollectBegin = "collect_begin"
	stageCollectExit  = "collect_exit"
	stagePostProbe    = "post_probe"
	stagePrune        = "prune"
	stageHusk         = "husk"
	stageModeFlip     = "mode_flip"
	stageStopped      = "stopped"
	stageFailure      = "failure"
	// stageHoldEngage/stageHoldRelease mirror the fence's
	// transition names: the run voices its own hold lines
	// through these, so one stream shows fence and collect
	// together. Rendering labels only — matching strings is
	// cosmetic, never logic.
	stageHoldEngage  = "hold_engage"
	stageHoldRelease = "hold_release"
)

// Collector runs the stock collector binary against the proven store,
// streaming its output and reporting the lifecycle.
type Collector func(ctx context.Context, out io.Writer, binPath string, args []string, report event.Reporter) error

// collect is the collector seam: the production implementation,
// swapped per test via useSeams. A constant function needs no port —
// nothing varies per run — so the seam carries the substitution
// alone (W13).
var collect Collector = runCollector

func fail(report event.Reporter, started time.Time, err error) error {
	failure := event.Timed(stageFailure, started)
	failure.Error = err.Error()
	event.Emit(report, failure)
	return err
}

// args builds the stock collector invocation: the operator's flags,
// nothing invented. dryRun previews (the default); only an explicit
// --no-dry-run collects for real.
func args(configPath string, deleteUntagged, dryRun bool) []string {
	args := []string{"garbage-collect"}
	if dryRun {
		args = append(args, "--dry-run")
	}
	if deleteUntagged {
		args = append(args, "--delete-untagged")
	}
	return append(args, configPath)
}

// collectorCommand is a seam for tests: it builds the *exec.Cmd that
// runs the stock registry collector. Tests replace it with a shell
// stub so the lifecycle runs without a registry anywhere near.
var collectorCommand = defaultCollectorCommand

func defaultCollectorCommand(ctx context.Context, binPath string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, binPath, args...)
}

type gcOutput struct {
	text string
	err  error
}

// scanGCOutput feeds merged collector lines to a channel: the stock
// binary cannot speak events, so its stdout/stderr lines ARE the
// feedback, streamed to the operator as they arrive.
func scanGCOutput(r io.Reader) <-chan gcOutput {
	output := make(chan gcOutput, 16)
	go func() {
		defer close(output)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			output <- gcOutput{text: scanner.Text()}
		}
		if err := scanner.Err(); err != nil {
			output <- gcOutput{err: err}
		}
	}()
	return output
}

// drainGCOutput blocks until scanGCOutput's goroutine has returned,
// signaled by its own close. runCollector never returns while that
// goroutine could still be mid-syscall on the pipe: a caller that got
// control back could otherwise reuse the pipe's fd number for an
// unrelated file, corrupting the in-flight read.
func drainGCOutput(lines <-chan gcOutput) {
	for range lines {
	}
}

// runCollector spawns the stock collector with its stdin closed and
// stdout/stderr merged into a pipe, streams every line to out, and
// reports the lifecycle. A failing exit carries the last line, so a
// number never arrives without the clue. Cancelling kills the child
// and reports stopped. Private: the collect seam above carries it,
// tests swap the seam.
func runCollector(ctx context.Context, out io.Writer, binPath string, args []string, report event.Reporter) error {
	started := time.Now()
	event.Emit(report, event.Event{Stage: stageStart})
	if err := ctx.Err(); err != nil {
		return fail(report, started, fmt.Errorf("collector: %w", err))
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fail(report, started, fmt.Errorf("collector: %w", err))
	}
	defer func() { _ = null.Close() }()
	reader, writer, err := os.Pipe()
	if err != nil {
		return fail(report, started, fmt.Errorf("collector: %w", err))
	}
	cmd := collectorCommand(ctx, binPath, args)
	cmd.Stdin = null
	cmd.Stdout = writer
	cmd.Stderr = writer

	event.Emit(report, event.Timed(stageSpawn, started))
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return fail(report, started, fmt.Errorf("collector: %w", err))
	}
	_ = writer.Close()
	begun := event.Timed(stageStarted, started)
	if cmd.Process != nil {
		begun.PID = cmd.Process.Pid
	}
	event.Emit(report, begun)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	lines := scanGCOutput(reader)
	defer drainGCOutput(lines)
	event.Emit(report, event.Timed(stageCollectBegin, started))
	lastLine := ""
	// feed streams one line to out, tracking the last for
	// failure context. Write errors kill the child and fail:
	// nobody is listening, collecting deaf helps no one.
	feed := func(line gcOutput) error {
		if line.err != nil {
			lastLine = line.err.Error()
			return nil
		}
		if strings.TrimSpace(line.text) != "" {
			lastLine = strings.TrimSpace(line.text)
		}
		_, werr := fmt.Fprintln(out, line.text)
		return werr
	}
	failOutput := func(werr error) error {
		killCollector(cmd)
		<-waitErr
		_ = reader.Close()
		return fail(report, started, fmt.Errorf("collector output: %w", werr))
	}
	// The exit and the stream race: a fast child is reaped while
	// its last lines still sit in the pipe. Returning on the exit
	// first would drop them, so the exit only ends the multiplex
	// phase — every line drains to out before this returns.
	var exitErr error
	exited, cancelled := false, false
	for !exited && !cancelled {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			if ferr := feed(line); ferr != nil {
				return failOutput(ferr)
			}
		case err := <-waitErr:
			exitErr = err
			exited = true
		case <-ctx.Done():
			cancelled = true
			killCollector(cmd)
		}
	}
	if cancelled {
		_ = reader.Close()
		<-waitErr
		event.Emit(report, event.Timed(stageStopped, started))
		return ctx.Err()
	}
	// The child is dead and every write end is closed, so the
	// scanner reaches EOF on its own: drain everything before
	// touching the reader — closing it first would abort the
	// in-flight read and drop whatever still sits in the pipe.
	if lines != nil {
		for line := range lines {
			if ferr := feed(line); ferr != nil {
				_ = reader.Close()
				return fail(report, started, fmt.Errorf("collector output: %w", ferr))
			}
		}
	}
	_ = reader.Close()
	exit := event.Timed(stageCollectExit, started)
	if exitErr != nil {
		exit.Error = exitErr.Error()
	}
	event.Emit(report, exit)
	if exitErr == nil {
		return nil
	}
	if lastLine != "" {
		exitErr = fmt.Errorf("%w: %s", exitErr, lastLine)
	}
	return fail(report, started, fmt.Errorf("garbage-collect: %w", exitErr))
}

func killCollector(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
