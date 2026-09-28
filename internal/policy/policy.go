// Package policy holds the programmatic cleanup behaviors and their
// tunings, colocated here — not in the main config. Per docs/PLAN.md
// these are client-side opinions evaluated by `kpr reap` (or any
// script): they mark rows due with a reason, they never delete.
// internal/config stays wiring-only (ports, redis addr, registry URL).
package policy

import (
	"regexp"
	"strconv"
	"time"
)

// Tunings for the ephemeral-tag behavior (docs/PLAN.md M2). DefaultTTL
// is the TTL non-matching tags fall back to; zero means no expiry, so
// a normal :latest is untouched. MaxTTL clamps every parsed TTL.
const (
	DefaultTTL = time.Duration(0)
	MaxTTL     = 30 * 24 * time.Hour
)

// ttlRe matches TTL tags in two forms: a bare ttl.sh-style number plus
// one unit (10m), or a CI commit build — a lowercase hex stem of any
// length plus a -ttl suffix (abc1234-10m). The stem group is
// non-capturing so submatch indices never move. Stems are lowercase
// hex only (what git emits); non-hex names like myapp-10m never match,
// while hex-spellable words (face-7d) inherently do.
var ttlRe = regexp.MustCompile(`^(?:[0-9a-f]+-)?(\d+)([smhdw])` + `$`)

// unitDur maps a TTL tag unit to its duration.
func unitDur(u byte) time.Duration {
	switch u {
	case 's':
		return time.Second
	case 'm':
		return time.Minute
	case 'h':
		return time.Hour
	case 'd':
		return 24 * time.Hour
	case 'w':
		return 7 * 24 * time.Hour
	}
	return 0
}

// parseTTL parses a TTL tag into its duration. ok is false when the tag
// carries no TTL encoding. A numeric part that overflows saturates to
// MaxTTL instead of wrapping (a wrapped-negative duration would expire
// immediately — the opposite of a huge TTL's intent).
func parseTTL(tag string) (ttl time.Duration, ok bool) {
	m := ttlRe.FindStringSubmatch(tag)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return MaxTTL, true
	}
	unit := unitDur(m[2][0])
	if unit == 0 {
		return 0, false
	}
	if n > uint64(MaxTTL/unit) {
		return MaxTTL, true
	}
	return time.Duration(n) * unit, true
}

// EffectiveTTL resolves a tag to its TTL: the parsed value clamped to
// MaxTTL, or the DefaultTTL fallback for non-matching tags. ok is false
// when the tag never expires (empty default), so callers treat it as
// keep without comparing durations.
func EffectiveTTL(tag string) (ttl time.Duration, ok bool) {
	ttl, matched := parseTTL(tag)
	if !matched {
		if DefaultTTL == 0 {
			return 0, false
		}
		return DefaultTTL, true
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	return ttl, true
}

// Eligible reports whether a row pushed at pushedAt is due for
// collection now. Eligibility anchors at push time: a late reap run
// must not grant extra life. A zero push time (unknown age) defaults
// to keep, and non-TTL tags are never eligible.
func Eligible(tag string, pushedAt, now time.Time) bool {
	ttl, ok := EffectiveTTL(tag)
	if !ok {
		return false
	}
	if pushedAt.IsZero() {
		return false
	}
	return !pushedAt.Add(ttl).After(now)
}
