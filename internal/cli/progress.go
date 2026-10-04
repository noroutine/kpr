package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// liveInterval is the repaint floor: faster runs still paint at
// most this often, so a 17k-tag walk doesn't redraw per tag.
const liveInterval = 300 * time.Millisecond

// liveLines repaints one \r line for long runs: counters, not a
// tens-of-thousands-line scroll. One cursor-up escape, no libs —
// carriage return plus space padding works on dumb terminals
// too. Each row pads only to its own previous width (never a
// global max — inflated trailing spaces alone wrap narrow
// terminals and glue the block). Pipes and files get nothing
// until done (no control codes in logs): only a char device
// repaints.
type liveLines struct {
	w     io.Writer
	tty   bool
	last  time.Time
	prev  []int
	block bool
}

// newLiveLines wraps w: repainting enables itself exactly when w
// is a terminal.
func newLiveLines(w io.Writer) *liveLines {
	tty := false
	if f, ok := w.(*os.File); ok {
		if st, err := f.Stat(); err == nil {
			tty = st.Mode()&os.ModeCharDevice != 0
		}
	}
	return &liveLines{w: w, tty: tty}
}

// terminal reports whether repaints are live: callers flush cheap
// early views straight to pipes, where nothing repaints.
func (l *liveLines) terminal() bool {
	return l.tty
}

// breakLine ends an in-progress repaint so an inline note starts
// on its own line; the next tick repaints the block fresh. A
// no-op off-terminal and when nothing is painted.
func (l *liveLines) breakLine() {
	if !l.tty || !l.block {
		return
	}
	_, _ = fmt.Fprintln(l.w)
	l.block = false
}

// tickBlock repaints an N-line block when due; doneBlock settles
// it. The first paint prints every line; later paints step back
// by what was painted and overwrite, scrubbing shrunk rows. One
// escape, no libs. Pipes and
// files get nothing until doneBlock, which prints lines[head:] —
// leading lines the caller already flushed (the cheap fast view,
// printed right away) are not repeated. The block is the
// display, never duplicated by a trailing summary.
func (l *liveLines) tickBlock(lines []string) {
	if !l.tty {
		return
	}
	now := time.Now()
	if now.Sub(l.last) < liveInterval {
		return
	}
	l.last = now
	l.paintBlock(lines)
}

func (l *liveLines) doneBlock(lines []string, head int) {
	if !l.tty {
		for _, s := range lines[head:] {
			_, _ = fmt.Fprintln(l.w, s)
		}
		return
	}
	l.paintBlock(lines)
	_, _ = fmt.Fprintln(l.w)
}

// widths counts terminal columns per row: runes, not bytes, so
// a multibyte mark like Δ pads one column, not three.
func widths(lines []string) []int {
	w := make([]int, len(lines))
	for i, s := range lines {
		w[i] = len([]rune(s))
	}
	return w
}

func (l *liveLines) paintBlock(lines []string) {
	cur := widths(lines)
	if !l.block {
		// First paint: no stale rows below, print raw.
		_, _ = fmt.Fprint(l.w, strings.Join(lines, "\n"))
		l.block = true
		l.prev = cur
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\r\x1b[%dA\r", len(l.prev)-1)
	for i, s := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
		if i < len(l.prev) && cur[i] < l.prev[i] {
			b.WriteString(strings.Repeat(" ", l.prev[i]-cur[i]))
		}
	}
	// Scrub stale rows below a shrunk block with blanks.
	for i := len(lines); i < len(l.prev); i++ {
		b.WriteString("\n" + strings.Repeat(" ", l.prev[i]))
	}
	_, _ = fmt.Fprint(l.w, b.String())
	l.prev = append(cur, make([]int, max(0, len(l.prev)-len(cur)))...)
}
