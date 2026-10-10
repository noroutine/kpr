// Package human holds the human magnitudes every command
// renders: compact ages and byte sizes, nothing else.
// Command-specific renderers stay with their commands — this
// package grows only when a second command measures the same
// way.
package human

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// durRe splits a Go duration rendering into its h/m/s parts so
// Dur can drop trailing zero components structurally: suffix
// trimming cannot tell the 0 of "0m" from the 0 of "10m".
var durRe = regexp.MustCompile(`^(-)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$`)

// Dur renders a duration without trailing zero units: "48h0m0s"
// reads "48h", "10m0s" reads "10m". Components rebuild from the
// parsed parts, so a real zero inside seconds ("1m30s", "10s")
// survives. All-zero stays "0s"; anything unparseable passes
// through untouched, never empty.
// NOTE(mutants): suffix-trimming ("m0s", "0s") passes round-hour
// cases yet eats "2h0m0s" into "2h0" and "1m30s" into "1m3" —
// the zero-minute and thirty-second cases pin the structure.
func Dur(d time.Duration) string {
	m := durRe.FindStringSubmatch(d.String())
	if m == nil {
		return d.String()
	}
	out := m[1]
	parts := []string{}
	if m[2] != "" {
		parts = append(parts, m[2]+"h")
	}
	if m[3] != "" {
		parts = append(parts, m[3]+"m")
	}
	if m[4] != "" {
		parts = append(parts, m[4]+"s")
	}
	// The loop keeps at least one part, and the regex matched at
	// least one (String never renders ""), so the join is never
	// empty: all-zero stays "0s" via the single surviving part.
	for len(parts) > 1 && (parts[len(parts)-1] == "0s" || parts[len(parts)-1] == "0m") {
		parts = parts[:len(parts)-1]
	}
	return out + strings.Join(parts, "")
}

// Ago renders a duration as a compact age ("2h5m ago"). A
// negative duration clamps to zero — the measurement is odd,
// the rendering must not be.
// NOTE(mutants): <= is equivalent — clamping an exactly-zero
// age to zero is identity.
func Ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return Dur(d.Round(time.Second)) + " ago"
}

// ShortAge renders the age of then at now as a compact
// duration ("2h5m ago").
func ShortAge(now, then time.Time) string {
	return Ago(now.Sub(then))
}

// Bytes renders bytes in the largest binary unit that keeps the
// value at one or more whole units, two decimals: bytes stay
// bytes, gibibytes stay gibibytes. Exact bytes stay in --json,
// humans get a sense of scale at any magnitude.
func Bytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	v := float64(b)
	unit := "B"
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		v /= 1024
		unit = u
		if v < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.2f %s", v, unit)
}
