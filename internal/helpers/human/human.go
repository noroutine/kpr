// Package human holds the human magnitudes every command
// renders: compact ages and byte sizes, nothing else.
// Command-specific renderers stay with their commands — this
// package grows only when a second command measures the same
// way.
package human

import (
	"fmt"
	"time"
)

// Ago renders a duration as a compact age ("2h5m0s ago"). A
// negative duration clamps to zero — the measurement is odd,
// the rendering must not be.
// NOTE(mutants): <= is equivalent — clamping an exactly-zero
// age to zero is identity.
func Ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String() + " ago"
}

// ShortAge renders the age of then at now as a compact
// duration ("2h5m0s ago").
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
