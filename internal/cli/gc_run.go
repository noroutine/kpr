package cli

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
	GCStageStart        = "start"
	GCStageSpawn        = "spawn"
	GCStageStarted      = "started"
	GCStagePreProbe     = "pre_probe"
	GCStageCollectBegin = "collect_begin"
	GCStageCollectExit  = "collect_exit"
	GCStagePostProbe    = "post_probe"
	GCStageModeFlip     = "mode_flip"
	GCStageStopped      = "stopped"
	GCStageFailure      = "failure"
)

// GCEvent is one lifecycle stage of a gc run. The JSON tags keep it
// suitable for the same JSON-lines transport the sweeper reports on,
// should the console ever subscribe.
type GCEvent struct {
	Stage     string `json:"stage"`
	ElapsedMs int64  `json:"elapsed_ms"`
	PID       int    `json:"pid,omitempty"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
}

// GCReporter receives gc lifecycle events. Nil reporters are fine:
// runCollector and runGC both check before emitting.
type GCReporter func(GCEvent)

func timedGCEvent(stage string, started time.Time) GCEvent {
	return GCEvent{Stage: stage, ElapsedMs: time.Since(started).Milliseconds()}
}

func emitGC(report GCReporter, event GCEvent) {
	if report != nil {
		report(event)
	}
}

func failGC(report GCReporter, started time.Time, err error) error {
	event := timedGCEvent(GCStageFailure, started)
	event.Error = err.Error()
	emitGC(report, event)
	return err
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
// and reports stopped.
func runCollector(ctx context.Context, out io.Writer, binPath string, args []string, report GCReporter) error {
	started := time.Now()
	emitGC(report, GCEvent{Stage: GCStageStart})
	if err := ctx.Err(); err != nil {
		return failGC(report, started, fmt.Errorf("collector: %w", err))
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return failGC(report, started, fmt.Errorf("collector: %w", err))
	}
	defer func() { _ = null.Close() }()
	reader, writer, err := os.Pipe()
	if err != nil {
		return failGC(report, started, fmt.Errorf("collector: %w", err))
	}
	cmd := collectorCommand(ctx, binPath, args)
	cmd.Stdin = null
	cmd.Stdout = writer
	cmd.Stderr = writer

	emitGC(report, timedGCEvent(GCStageSpawn, started))
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return failGC(report, started, fmt.Errorf("collector: %w", err))
	}
	_ = writer.Close()
	begun := timedGCEvent(GCStageStarted, started)
	if cmd.Process != nil {
		begun.PID = cmd.Process.Pid
	}
	emitGC(report, begun)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	lines := scanGCOutput(reader)
	defer drainGCOutput(lines)
	emitGC(report, timedGCEvent(GCStageCollectBegin, started))
	lastLine := ""
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			if line.err != nil {
				lastLine = line.err.Error()
				continue
			}
			if strings.TrimSpace(line.text) != "" {
				lastLine = strings.TrimSpace(line.text)
			}
			if _, werr := fmt.Fprintln(out, line.text); werr != nil {
				killCollector(cmd)
				<-waitErr
				_ = reader.Close()
				return failGC(report, started, fmt.Errorf("collector output: %w", werr))
			}
		case err := <-waitErr:
			_ = reader.Close()
			exit := timedGCEvent(GCStageCollectExit, started)
			if err != nil {
				exit.Error = err.Error()
			}
			emitGC(report, exit)
			if err == nil {
				return nil
			}
			if lastLine != "" {
				err = fmt.Errorf("%w: %s", err, lastLine)
			}
			return failGC(report, started, fmt.Errorf("garbage-collect: %w", err))
		case <-ctx.Done():
			killCollector(cmd)
			<-waitErr
			_ = reader.Close()
			emitGC(report, timedGCEvent(GCStageStopped, started))
			return ctx.Err()
		}
	}
}

func killCollector(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
