package policy

import (
	"regexp"
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

// Suffixed tags carry an explicit TTL: any alphanumeric+hyphen stem
// of any length plus number+unit (abc1234-10m, myapp-10m). The stem
// carries no meaning, so scoping it was scoping the intent —
// dropped. What stays out is anything without the suffix (dotted
// versions, bare words). If this fails, explicit TTLs never expire
// or suffix-less tags get eaten.
func TestEffectiveTTLCommitHashSuffix(t *testing.T) {
	matched := map[string]time.Duration{
		"abc1234-10m": 10 * time.Minute,
		"abc123-10m":  10 * time.Minute,
		"1234567-2h":  2 * time.Hour,
		"facade-7d":   7 * 24 * time.Hour,
		"fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e-30s": 30 * time.Second,
		// Relaxed stems: the suffix is the explicit intent, so any
		// alphanumeric+hyphen stem of any length matches.
		"myapp-10m":    10 * time.Minute,
		"release-7d":   7 * 24 * time.Hour,
		"ABC1234-10m":  10 * time.Minute,
		"a-1h":         1 * time.Hour,
		"abc12-10m":    10 * time.Minute,
		"face-7d":      7 * 24 * time.Hour,
		"feature-x-2d": 2 * 24 * time.Hour,
	}
	for tag, want := range matched {
		got, ok := EffectiveTTL(tag)
		if !ok {
			t.Errorf("EffectiveTTL(%q) not matched, want %v", tag, want)
			continue
		}
		if got != want {
			t.Errorf("EffectiveTTL(%q) = %v, want %v", tag, got, want)
		}
	}
	// The suffix is the explicit intent, so the stem shape no longer
	// refuses: what stays out is anything without number+unit at the
	// end — dotted versions, dangling hyphens, unit-less stems.
	for _, tag := range []string{"v1.2.3-1h", "abc1234-", "10m-", "latest", "myapp", "10x", "10M"} {
		if ttl, ok := EffectiveTTL(tag); ok {
			t.Errorf("EffectiveTTL(%q) = (%v, true), want (0, false)", tag, ttl)
		}
	}
}

// A bare commit hash (no -ttl suffix) gets the hash default: 48h of
// implied TTL, 24h expired builds too fast. All-digit tags
// read as hashes too unless they are date-like — unknown intent
// defaults to collect, dates default to keep. If this fails, commit
// builds pile up forever or date tags get eaten.
func TestEffectiveTTLHashDefault48h(t *testing.T) {
	for _, tag := range []string{"abc1234", "abcdef", "deadbee", "fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e", "123456", "1234567", "1754123456"} {
		got, ok := EffectiveTTL(tag)
		if !ok {
			t.Errorf("EffectiveTTL(%q) not matched, want 48h", tag)
			continue
		}
		if got != 48*time.Hour {
			t.Errorf("EffectiveTTL(%q) = %v, want 48h", tag, got)
		}
	}
	for _, tag := range []string{"20240115", "myapp", "latest", "ABC1234", "abc12", "12345"} {
		if ttl, ok := EffectiveTTL(tag); ok {
			t.Errorf("EffectiveTTL(%q) = (%v, true), want (0, false)", tag, ttl)
		}
	}
}

// Date-like all-digit tags are spared from the hash default:
// YYYYMMDD with a real calendar date inside the Docker-era
// window, plus up to two DNS-serial counter digits. Anything
// else all-digit — out-of-range years, impossible months,
// build counters — reads as a hash. If this fails, date-tagged
// releases get eaten or number soup piles up forever.
func TestDateLikeTagsSpared(t *testing.T) {
	for _, tag := range []string{"20240115", "20240229", "20130301", "21130301", "202401151", "2024011512"} {
		if ttl, ok := EffectiveTTL(tag); ok {
			t.Errorf("EffectiveTTL(%q) = (%v, true), want (0, false)", tag, ttl)
		}
	}
	for _, tag := range []string{"19681534", "19990101", "20241301", "20240230", "21130302", "99999999"} {
		got, ok := EffectiveTTL(tag)
		if !ok || got != 48*time.Hour {
			t.Errorf("EffectiveTTL(%q) = (%v, %v), want (48h, true)", tag, got, ok)
		}
	}
}

// The bare-hash letter check is boundary-exact: 'a' and 'f' count,
// 'g' does not. A hash whose only letters sit on the boundary
// (a12345, f12345) must still read as a hash. If this fails, an
// off-by-one reclassifies boundary hashes as build numbers and they
// lose their 48h default.
func TestBareHashLetterBoundaries(t *testing.T) {
	for _, tag := range []string{"a12345", "f12345", "a1b2c3", "f9e8d7"} {
		got, ok := EffectiveTTL(tag)
		if !ok || got != 48*time.Hour {
			t.Errorf("EffectiveTTL(%q) = (%v, %v), want (48h, true)", tag, got, ok)
		}
	}
	if ttl, ok := EffectiveTTL("g12345"); ok {
		t.Errorf("EffectiveTTL(g12345) = (%v, true), want (0, false)", ttl)
	}
}

// A TTL regex edit that drops a capture group must degrade to no-match
// (keep), never to an index panic mid-reap. If this fails, the next
// person to touch ttlRe can crash every reap run.
func TestParseTTLToleratesShortSubmatch(t *testing.T) {
	old := ttlRe
	ttlRe = regexp.MustCompile(`^(\d+)$`)
	defer func() { ttlRe = old }()
	if ttl, ok := parseTTL("123"); ok {
		t.Errorf("parseTTL(123) = (%v, true) under a groupless regex, want (0, false)", ttl)
	}
}

// A leading hyphen is not a negative TTL and not an empty stem:
// "-14m" and friends default to keep. Docker tags cannot open
// with a hyphen either, so nothing reachable parses this way —
// the rule just refuses to read intent into stray hyphens. If
// this fails, a stray-hyphen tag silently becomes a live TTL.
func TestLeadingHyphenTagsDefaultKeep(t *testing.T) {
	for _, tag := range []string{"-14m", "--14m", "-10s", "--1h-30m"} {
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
	// Literals, not MaxTTL: a symbolic assert moves with the tuning
	// and pins nothing. The max is thirty days.
	const max = 30 * 24 * time.Hour
	if ttl, ok := EffectiveTTL("100000h"); !ok || ttl != max {
		t.Errorf("EffectiveTTL(100000h) = (%v, %v), want (%v, true)", ttl, ok, max)
	}
	// 100 weeks is 700 days: the clamp, not the calendar, wins.
	if ttl, ok := EffectiveTTL("100w"); !ok || ttl != max {
		t.Errorf("EffectiveTTL(100w) = (%v, %v), want (%v, true)", ttl, ok, max)
	}
	if ttl, ok := EffectiveTTL("99999999999999999999w"); !ok || ttl != max {
		t.Errorf("EffectiveTTL(huge) = (%v, %v), want (%v, true)", ttl, ok, max)
	}
}

// Exactly-at-max passes through unclamped while one unit over clamps:
// the clamp boundary must be exact, not approximate. If this fails,
// a max-sized TTL either shrinks silently or an over-max one slips by.
func TestEffectiveTTLMaxBoundaryExact(t *testing.T) {
	// Literals, not MaxTTL: a symbolic assert moves with the tuning
	// and pins nothing. The max is thirty days.
	const max = 30 * 24 * time.Hour
	if ttl, ok := EffectiveTTL("720h"); !ok || ttl != max {
		t.Errorf("EffectiveTTL(720h) = (%v, %v), want (%v, true) unclamped", ttl, ok, max)
	}
	if ttl, ok := EffectiveTTL("721h"); !ok || ttl != max {
		t.Errorf("EffectiveTTL(721h) = (%v, %v), want (%v, true) clamped", ttl, ok, max)
	}
}

// Four weeks is 672h — under the max, so no clamping: the clamp
// quotient truncates, and exactly-quotient units pass through whole.
// The literal pins the > (not >=) boundary: with >=, 4w would clamp
// to the 720h max instead of its own 672h. If this fails, whole
// sub-max durations shrink or grow at the clamp edge.
func TestEffectiveTTLWholeWeeksPassThrough(t *testing.T) {
	if ttl, ok := EffectiveTTL("4w"); !ok || ttl != 4*7*24*time.Hour {
		t.Errorf("EffectiveTTL(4w) = (%v, %v), want (672h, true)", ttl, ok)
	}
}

// The unit map covers exactly s/m/h/d/w: anything else is no unit,
// never a default. If this fails, a typo'd unit parses as seconds
// (or panics the lookup).
func TestUnitDurMapsKnownUnits(t *testing.T) {
	for _, tc := range []struct {
		unit byte
		want time.Duration
	}{
		{'s', time.Second},
		{'m', time.Minute},
		{'h', time.Hour},
		{'d', 24 * time.Hour},
		{'w', 7 * 24 * time.Hour},
		{'x', 0},
		{'S', 0},
	} {
		if got := unitDur(tc.unit); got != tc.want {
			t.Errorf("unitDur(%q) = %v, want %v", tc.unit, got, tc.want)
		}
	}
}

// A regex edit that admits unknown units must degrade to no-match:
// the unit gate, not the regex, owns the vocabulary. If this fails,
// a widened regex parses "10x" as a real TTL.
func TestParseTTLUnknownUnitRefuses(t *testing.T) {
	old := ttlRe
	ttlRe = regexp.MustCompile(`^(\d+)([a-z])$`)
	defer func() { ttlRe = old }()
	if ttl, ok := parseTTL("10x"); ok {
		t.Errorf("parseTTL(10x) = (%v, true) under a widened regex, want (0, false)", ttl)
	}
}

// NOTE: no test sets a non-zero DefaultTTL — it is a const 0, so
// the `return DefaultTTL, true` fallback is dormant by build, not
// by neglect. Flipping the const needs the branch back under test
// the same day.

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
