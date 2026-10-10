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
		{"hours", 2*time.Hour + 5*time.Minute, "2h5m ago"},
		{"exact hours", 48 * time.Hour, "48h ago"},
		{"zero", 0, "0s ago"},
		{"future clamps", -time.Hour, "0s ago"},
	} {
		if got := human.Ago(tc.d); got != tc.want {
			t.Errorf("Ago(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := human.ShortAge(now, now.Add(-90*time.Minute)); got != "1h30m ago" {
		t.Errorf("ShortAge(90m) = %q, want 1h30m ago", got)
	}
	if got := human.ShortAge(now, now.Add(time.Hour)); got != "0s ago" {
		t.Errorf("ShortAge(future) = %q, want 0s ago", got)
	}
}

// Dur drops trailing zero units ("48h0m0s" reads "48h", "10m0s"
// reads "10m") while a bare "0s" and a seconds-only "10s" stay
// whole — trimming those would eat real digits. If this fails,
// reasons and ages print Go-String tails again.
func TestDurDropsZeroTails(t *testing.T) {
	for _, c := range []struct {
		in   time.Duration
		want string
	}{
		{48 * time.Hour, "48h"},
		{24 * time.Hour, "24h"},
		{168 * time.Hour, "168h"},
		{10 * time.Minute, "10m"},
		{2*time.Hour + 5*time.Minute, "2h5m"},
		{90 * time.Minute, "1h30m"},
		{30 * time.Second, "30s"},
		{10 * time.Second, "10s"},
		{90 * time.Second, "1m30s"},
		{0, "0s"},
		{1*time.Hour + 2*time.Minute + 3*time.Second, "1h2m3s"},
		{1*time.Hour + 5*time.Second, "1h0m5s"},
		{-90 * time.Minute, "-1h30m"},
		{1500 * time.Millisecond, "1.5s"},
		{500 * time.Millisecond, "500ms"},
	} {
		if got := human.Dur(c.in); got != c.want {
			t.Errorf("Dur(%v) = %q, want %q", c.in, got, c.want)
		}
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

// Stamps read human beside the exact value: the operator glances
// ("5m ago") without losing precision. Zero degrades to a word,
// future clamps like every Ago. If this fails, displays fall back
// to timestamp arithmetic.
func TestAgeReadsLikeShortAge(t *testing.T) {
	now := time.Now().UTC()
	if got := human.Age(now.Add(-90 * time.Second)); got != "1m30s ago" {
		t.Errorf("Age(-90s) = %q, want 1m30s ago", got)
	}
	if got := human.Age(now.Add(time.Hour)); got != "0s ago" {
		t.Errorf("Age(future) = %q, want clamped 0s ago", got)
	}
	if got := human.Age(time.Time{}); got != "unknown" {
		t.Errorf("Age(zero) = %q, want unknown", got)
	}
}
