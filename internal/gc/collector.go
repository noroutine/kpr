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
)

// GC lifecycle stages. Probes and proofs report through these alongside
// the collector itself, so one event stream tells the whole run: what
// the sentinel saw before, what the collector said, what it saw after.
const (
	StageStart        = "start"
	StageSpawn        = "spawn"
	StageStarted      = "started"
	StagePreProbe     = "pre_probe"
	StageCollectBegin = "collect_begin"
	StageCollectExit  = "collect_exit"
	StagePostProbe    = "post_probe"
	StageModeFlip     = "mode_flip"
	StageStopped      = "stopped"
	StageFailure      = "failure"
)

// Event is one lifecycle stage of a gc run. The JSON tags keep it
// suitable for the same JSON-lines transport the sweeper reports on,
// should the console ever subscribe.
type Event struct {
	Stage     string `json:"stage"`
	ElapsedMs int64  `json:"elapsed_ms"`
	PID       int    `json:"pid,omitempty"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Reporter receives gc lifecycle events. Nil reporters are fine:
// RunCollector and the run orchestration both check before emitting.
type Reporter func(Event)

// Collector runs the stock collector binary against the proven store,
// streaming its output and reporting the lifecycle. RunCollector is
// the production implementation; tests substitute a stub. Consumed by
// the run orchestration when it moves (gc-4).
type Collector func(ctx context.Context, out io.Writer, binPath string, args []string, report Reporter) error

// RunCollector satisfies Collector: the assertion pins the port to
// the implementation it will carry.
var _ Collector = RunCollector

// Timed stamps one lifecycle event against the run start.
func Timed(stage string, started time.Time) Event {
	return Event{Stage: stage, ElapsedMs: time.Since(started).Milliseconds()}
}

// Emit delivers one lifecycle event. Nil reporters are fine.
func Emit(report Reporter, event Event) {
	if report != nil {
		report(event)
	}
}

func fail(report Reporter, started time.Time, err error) error {
	event := Timed(StageFailure, started)
	event.Error = err.Error()
	Emit(report, event)
	return err
}

// Args builds the stock collector invocation: the operator's flags,
// nothing invented. dryRun previews (the default); only an explicit
// --no-dry-run collects for real.
func Args(configPath string, deleteUntagged, dryRun bool) []string {
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
// signaled by its own close. RunCollector never returns while that
// goroutine could still be mid-syscall on the pipe: a caller that got
// control back could otherwise reuse the pipe's fd number for an
// unrelated file, corrupting the in-flight read.
func drainGCOutput(lines <-chan gcOutput) {
	for range lines {
	}
}

// RunCollector spawns the stock collector with its stdin closed and
// stdout/stderr merged into a pipe, streams every line to out, and
// reports the lifecycle. A failing exit carries the last line, so a
// number never arrives without the clue. Cancelling kills the child
// and reports stopped.
func RunCollector(ctx context.Context, out io.Writer, binPath string, args []string, report Reporter) error {
	started := time.Now()
	Emit(report, Event{Stage: StageStart})
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

	Emit(report, Timed(StageSpawn, started))
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return fail(report, started, fmt.Errorf("collector: %w", err))
	}
	_ = writer.Close()
	begun := Timed(StageStarted, started)
	if cmd.Process != nil {
		begun.PID = cmd.Process.Pid
	}
	Emit(report, begun)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	lines := scanGCOutput(reader)
	defer drainGCOutput(lines)
	Emit(report, Timed(StageCollectBegin, started))
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
		Emit(report, Timed(StageStopped, started))
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
	exit := Timed(StageCollectExit, started)
	if exitErr != nil {
		exit.Error = exitErr.Error()
	}
	Emit(report, exit)
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
