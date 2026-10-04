package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// breakLine ends an in-progress repaint so an inline note starts
// on its own line, and clears the block so the next tick
// repaints fresh. Off-terminal and with nothing painted it is a
// silent no-op. If this fails, mid-run warnings glue onto live
// counters.
func TestLiveLinesBreakLine(t *testing.T) {
	var buf bytes.Buffer
	live := &liveLines{w: &buf, tty: true, block: true}
	live.breakLine()
	if buf.String() != "\n" {
		t.Errorf("breakLine wrote %q, want one newline", buf.String())
	}
	live.breakLine()
	if buf.String() != "\n" {
		t.Errorf("second breakLine wrote %q, want no-op once cleared", buf.String())
	}
	quiet := &liveLines{w: &buf, tty: false, block: true}
	quiet.breakLine()
	if buf.String() != "\n" {
		t.Errorf("off-terminal breakLine wrote %q, want passthrough silence", buf.String())
	}
}

// Repaints pad each row only to its own previous width: padding
// to the global max inflates short rows past narrow terminals,
// and the wrap glues the block. If this fails, trailing spaces
// alone break the refresh.
func TestLiveLinesPadsPerRow(t *testing.T) {
	var buf bytes.Buffer
	live := &liveLines{w: &buf, tty: true}
	live.paintBlock([]string{"abcdefgh", "xy"})
	live.paintBlock([]string{"ab", "xyz"})
	want := "abcdefgh\nxy" + "\r\x1b[1A\rab      \nxyz"
	if buf.String() != want {
		t.Errorf("repaint = %q, want %q", buf.String(), want)
	}
}

// Pipes never repaint: only a char device earns control codes,
// so piped logs stay clean until done. If this fails, piped runs
// emit escapes.
func TestLiveLinesPipeStaysDark(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = pr.Close(); _ = pw.Close() }()
	live := newLiveLines(pw)
	live.last = time.Now().Add(-time.Hour)
	live.tickBlock([]string{"a"})
	_ = pw.Close()
	got, _ := io.ReadAll(pr)
	if len(got) != 0 {
		t.Errorf("pipe got %q, want silence until done", got)
	}
}

// A single-line block repaints with carriage return alone: a
// zero-count cursor-up reads as up-one on real terminals
// (ECMA-48 defaults the parameter to 1), so every tick would
// climb a row. If this fails, one-line counters print upwards.
func TestLiveLinesSingleLineStaysPut(t *testing.T) {
	var buf bytes.Buffer
	live := &liveLines{w: &buf, tty: true}
	live.paintBlock([]string{"aa"})
	live.paintBlock([]string{"b"})
	if got := buf.String(); got != "aa\rb " {
		t.Errorf("paints = %q, want one line overwritten in place", got)
	}
}

// Throttled ticks paint on terminals: the first tick draws the
// block, a tick inside the interval draws nothing, and done
// settles with a newline. If this fails, terminal runs show no
// live line (or redraw every row).
func TestLiveLinesTickPaintsAndThrottles(t *testing.T) {
	var buf bytes.Buffer
	live := &liveLines{w: &buf, tty: true}
	live.tickBlock([]string{"sweep 1: 1 performed, 0 planned"})
	if !strings.Contains(buf.String(), "1 performed") {
		t.Fatalf("first tick painted %q, want the line", buf.String())
	}
	n := len(buf.String())
	live.tickBlock([]string{"sweep 1: 2 performed, 0 planned"})
	if len(buf.String()) != n {
		t.Errorf("throttled tick repainted, want silence inside the interval")
	}
	live.doneBlock([]string{"sweep 1: 2 performed, 0 planned"}, 0)
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("done block = %q, want a settling newline", buf.String())
	}
}

// A block that grows or shrinks steps back by what was painted,
// scrubbing stale rows with blanks. If this fails, mid-run view
// changes glue onto old rows.
func TestLiveLinesRepaintsResizedBlock(t *testing.T) {
	var buf bytes.Buffer
	live := &liveLines{w: &buf, tty: true}
	live.paintBlock([]string{"aa", "bb"})
	live.paintBlock([]string{"cc", "dd", "ee", "ff"})
	live.paintBlock([]string{"gg"})
	want := "aa\nbb" +
		"\r\x1b[1A\rcc\ndd\nee\nff" +
		"\r\x1b[3A\rgg\n  \n  \n  "
	if buf.String() != want {
		t.Errorf("repaint = %q, want %q", buf.String(), want)
	}
}
