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

// Skew always carries its sign: ahead reads +, behind reads -,
// zero reads +0s. If this fails, the clock card flips
// behind/ahead.
func TestSignedOffsetCarriesSign(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "+5s"},
		{-5 * time.Second, "-5s"},
		{0, "+0s"},
		{83 * time.Millisecond, "+83ms"},
	} {
		if got := human.SignedOffset(tc.d); got != tc.want {
			t.Errorf("SignedOffset(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// Capacity reads in operator glance bands (whole TB/GB above
// ten, one decimal below, MB under a gig) — beside Bytes'
// precise binary units, never instead. If this fails, the
// store card's free/total lie while the bytes are right.
func TestCapacityReadsGlanceBands(t *testing.T) {
	const kb = 1024
	for _, tc := range []struct {
		n    uint64
		want string
	}{
		{500 * 1024 * 1024, "500 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{5 * 1024 * 1024 * 1024, "5.0 GB"},
		{10 * 1024 * 1024 * 1024, "10 GB"},
		{91 * 1024 * 1024 * 1024, "91 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
		{6300 * 1024 * 1024 * 1024, "6.2 TB"},
		{10 * 1024 * 1024 * 1024 * 1024, "10 TB"},
		{10*1024*1024*1024*1024 - kb, "10.0 TB"},
	} {
		if got := human.Capacity(tc.n); got != tc.want {
			t.Errorf("Capacity(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// Used percent is whole percent used, free==total reading
// zero. If this fails, the card's percent lies while free and
// total read true.
func TestUsedPercentReadsWholePercent(t *testing.T) {
	for _, tc := range []struct {
		total, free uint64
		want        uint64
	}{
		{100, 30, 70},
		{100, 100, 0},
		{100, 0, 100},
		{3, 1, 66},
	} {
		if got := human.UsedPercent(tc.total, tc.free); got != tc.want {
			t.Errorf("UsedPercent(%d, %d) = %d, want %d", tc.total, tc.free, got, tc.want)
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
