package human_test

import (
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/helpers/human"
)

// Ages read compact ("2h5m ago"), never negative: a future
// stamp clamps to zero — the row is odd, the rendering must
// not be. If this fails, clock-skewed pushes print negative
// ages.
func TestAgoRendersCompactAges(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		d    time.Duration
		want string
	}{
		{"hours", 2*time.Hour + 5*time.Minute, "2h5m0s ago"},
		{"zero", 0, "0s ago"},
		{"future clamps", -time.Hour, "0s ago"},
	} {
		if got := human.Ago(tc.d); got != tc.want {
			t.Errorf("Ago(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := human.ShortAge(now, now.Add(-90*time.Minute)); got != "1h30m0s ago" {
		t.Errorf("ShortAge(90m) = %q, want 1h30m0s ago", got)
	}
	if got := human.ShortAge(now, now.Add(time.Hour)); got != "0s ago" {
		t.Errorf("ShortAge(future) = %q, want 0s ago", got)
	}
}

// Bytes read in the largest binary unit that keeps the value at
// one or more whole units, two decimals: bytes stay bytes,
// gibibytes stay gibibytes. If this fails, magnitudes mislead.
func TestBytesScalesUnits(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{71, "71 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{9580388, "9.14 MiB"},
		{679001899008, "632.37 GiB"},
		{1 << 50, "1.00 PiB"},
	} {
		if got := human.Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
