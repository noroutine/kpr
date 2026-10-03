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
// tens-of-thousands-line scroll. No ANSI, no libs — carriage
// return plus space padding works on dumb terminals too. Pipes
// and files get nothing until done (no control codes in logs):
// only a char device repaints.
type liveLines struct {
	w     io.Writer
	tty   bool
	last  time.Time
	wide  int
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

// tick repaints when due; done paints the final line plus newline.
// Both are no-ops off-terminal except done's newline discipline:
// done always ends the line it may have started, so the summary
// that follows never glues onto a counter.
func (l *liveLines) tick(line string) {
	if !l.tty {
		return
	}
	now := time.Now()
	if now.Sub(l.last) < liveInterval {
		return
	}
	l.last = now
	l.paint(line)
}

func (l *liveLines) done(line string) {
	if !l.tty {
		return
	}
	l.paint(line)
	_, _ = fmt.Fprintln(l.w)
}

func (l *liveLines) paint(line string) {
	if len(line) > l.wide {
		l.wide = len(line)
	}
	_, _ = fmt.Fprintf(l.w, "\r%s%s", line, strings.Repeat(" ", l.wide-len(line)))
}

// tickTwo repaints a two-line block when due; doneTwo settles it.
// The first paint prints both lines; later paints step back up one
// line and overwrite. One escape, no libs. Pipes and files get
// nothing until doneTwo, which prints both lines once — the live
// block is the display, never duplicated by a trailing summary.
func (l *liveLines) tickTwo(first, second string) {
	if !l.tty {
		return
	}
	now := time.Now()
	if now.Sub(l.last) < liveInterval {
		return
	}
	l.last = now
	l.paintTwo(first, second)
}

func (l *liveLines) doneTwo(first, second string) {
	if !l.tty {
		_, _ = fmt.Fprintf(l.w, "%s\n%s\n", first, second)
		return
	}
	l.paintTwo(first, second)
	_, _ = fmt.Fprintln(l.w)
}

func (l *liveLines) paintTwo(first, second string) {
	for _, s := range []string{first, second} {
		if len(s) > l.wide {
			l.wide = len(s)
		}
	}
	pad := func(s string) string {
		return s + strings.Repeat(" ", l.wide-len(s))
	}
	if !l.block {
		_, _ = fmt.Fprintf(l.w, "%s\n%s", pad(first), pad(second))
		l.block = true
		return
	}
	_, _ = fmt.Fprintf(l.w, "\r\x1b[1A\r%s\n%s", pad(first), pad(second))
}
