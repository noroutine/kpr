package policy

import (
	"testing"
	"time"
)

// A scratch repo pushed as app:10m must resolve to a ten-minute TTL:
// below that age it stays, past it reap may mark it. If this fails,
// ephemeral tags never expire (or expire at the wrong age) and the M2
// promise — never wiped before, collectable whenever after — is broken.
func TestEffectiveTTLParsesAllUnits(t *testing.T) {
	cases := map[string]time.Duration{
		"30s": 30 * time.Second,
		"10m": 10 * time.Minute,
		"2h":  2 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"1w":  7 * 24 * time.Hour,
		"0s":  0,
	}
	for tag, want := range cases {
		got, ok := EffectiveTTL(tag)
		if !ok {
			t.Errorf("EffectiveTTL(%q) not matched, want %v", tag, want)
			continue
		}
		if got != want {
			t.Errorf("EffectiveTTL(%q) = %v, want %v", tag, got, want)
		}
	}
}

// Tags like :latest or :v1.2.3 carry no TTL encoding, so with the
// default empty they must never expire. If this fails, ordinary tags
// get swept — the exact data loss the empty-default tuning exists to
// prevent.
func TestEffectiveTTLNonMatchingTagNeverExpires(t *testing.T) {
	for _, tag := range []string{"latest", "v1.2.3", "", "10x", "h", "1h30m", " 10m", "10m "} {
		if ttl, ok := EffectiveTTL(tag); ok {
			t.Errorf("EffectiveTTL(%q) = (%v, true), want (0, false)", tag, ttl)
		}
	}
}

// A TTL larger than the colocated max must clamp to the max, and a
// numeric part that overflows int64 must saturate instead of wrapping
// negative (a wrapped duration would expire immediately — the opposite
// of a huge TTL's intent). If this fails, one fat-fingered tag wipes a
// repo on the next reap.
func TestEffectiveTTLClampsToMax(t *testing.T) {
	if ttl, ok := EffectiveTTL("100000h"); !ok || ttl != MaxTTL {
		t.Errorf("EffectiveTTL(100000h) = (%v, %v), want (%v, true)", ttl, ok, MaxTTL)
	}
	if ttl, ok := EffectiveTTL("99999999999999999999w"); !ok || ttl != MaxTTL {
		t.Errorf("EffectiveTTL(huge) = (%v, %v), want (%v, true)", ttl, ok, MaxTTL)
	}
}

// Eligibility anchors at the receiver-stamped push time, not at mark
// time: app:10m pushed at T is eligible at T+10m however late reap
// runs. If this fails, a late reap grants extra life (or an early one
// wipes before the minimal promise).
func TestEligibleAnchorsAtPushTime(t *testing.T) {
	pushed := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	if Eligible("10m", pushed, pushed.Add(9*time.Minute)) {
		t.Error("minute-9 pull candidate eligible, want kept (minimal promise)")
	}
	if !Eligible("10m", pushed, pushed.Add(10*time.Minute)) {
		t.Error("minute-10 candidate not eligible, want due")
	}
	if !Eligible("10m", pushed, pushed.Add(3*time.Hour)) {
		t.Error("long-overdue candidate not eligible, want due (whenever-after)")
	}
}

// Rows with no recorded push time (pre-kpr tags, partial rows) default
// to keep: an unknown age must never read as expired. If this fails,
// backfill gaps turn into mass deletions on the first reap.
func TestEligibleUnknownPushTimeKeeps(t *testing.T) {
	now := time.Now().UTC()
	if Eligible("10m", time.Time{}, now) {
		t.Error("zero push time eligible, want kept (unknown age defaults keep)")
	}
	if Eligible("10m", time.Time{}, now.Add(1000*time.Hour)) {
		t.Error("zero push time eligible far in the future, want kept")
	}
}

// A non-TTL tag is never eligible no matter how old the row gets —
// :latest pushed a year ago still pulls. If this fails, age alone
// becomes a deletion criterion and the M2 done-condition (:latest
// untouched) breaks.
func TestEligibleNonTTLTagNeverDue(t *testing.T) {
	pushed := time.Now().UTC().Add(-365 * 24 * time.Hour)
	if Eligible("latest", pushed, time.Now().UTC()) {
		t.Error("year-old :latest eligible, want kept")
	}
}
