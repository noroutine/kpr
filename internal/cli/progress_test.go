package cli

import (
	"bytes"
	"testing"
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
