// Package policy holds the programmatic cleanup behaviors and their
// tunings, colocated here — not in the main config. Per docs/ARCHITECTURE.md
// these are client-side opinions evaluated by `kpr reap` (or any
// script): they mark rows due with a reason, they never delete.
// internal/config stays wiring-only (ports, redis addr, registry URL).
package policy

import (
	"regexp"
	"strconv"
	"time"
)

// Tunings for the ephemeral-tag behavior (docs/ARCHITECTURE.md, Behaviors). DefaultTTL
// is the TTL non-matching tags fall back to; zero means no expiry, so
// a normal :latest is untouched. MaxTTL clamps every parsed TTL.
// HashTTL is the default for bare commit hashes (no -ttl suffix): 48h
// of implied TTL before they become eligible; 24h expired builds
// too fast.
const (
	DefaultTTL = time.Duration(0)
	MaxTTL     = 30 * 24 * time.Hour
	HashTTL    = 48 * time.Hour
)

// ttlRe matches TTL tags in two forms: a bare ttl.sh-style number plus
// one unit (10m), or a suffixed tag — an alphanumeric+hyphen stem of
// any length plus a -ttl suffix (abc1234-10m, myapp-10m). The stem
// opens with an alphanumeric, never a hyphen: leading hyphens are
// refused, not read as negatives ("-14m" defaults to keep — and
// docker tags cannot open with one either). The stem group is
// non-capturing so submatch indices never move. The suffix is the
// explicit intent, so the stem carries no meaning: hex-only scoping
// was dropped once CI tags stopped looking like git hashes.
// Bare tags (no suffix) still go through isBareHash below.
var ttlRe = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9-]*-)?(\d+)([smhdw])` + `$`)

// hashRe matches a bare commit hash: lowercase hex, at least six
// chars. The letter check lives in isBareHash: hex with at least
// one a-f letter reads as a hash outright, all-digit hex only
// when it is not date-like.
var hashRe = regexp.MustCompile(`^[0-9a-f]{6,}$`)

// dateFloor and dateCeil bound date-like tags: Docker's first public
// release month to a century out. An all-digit tag outside the
// window cannot be a release date, so it reads as a hash.
var (
	dateFloor = time.Date(2013, 3, 1, 0, 0, 0, 0, time.UTC)
	dateCeil  = dateFloor.AddDate(100, 0, 0)
)

// isDateLike reports whether tag is YYYYMMDD with a real calendar
// date inside the Docker-era window, plus up to two DNS-serial
// counter digits. time.Parse rejects impossible months and days
// (month 13, Feb 30); the window rejects years like 1968.
func isDateLike(tag string) bool {
	if len(tag) != 8 && len(tag) != 9 && len(tag) != 10 {
		return false
	}
	// NOTE(mutants): reached only via isBareHash today (hashRe
	// plus the letter scan already guarantee digits), but the
	// guard keeps isDateLike correct standalone — dropping it
	// lets "2024ab15" read as a date for any future caller.
	for i := 0; i < len(tag); i++ {
		if tag[i] < '0' || tag[i] > '9' {
			return false
		}
	}
	d, err := time.Parse("20060102", tag[:8])
	if err != nil {
		return false
	}
	return !d.Before(dateFloor) && !d.After(dateCeil)
}

// isBareHash reports whether tag is a commit hash without an explicit
// -ttl suffix: hex of at least six with at least one a-f letter, or
// all-digit hex that is not date-like. Date-like tags default to
// keep; the rest of number-only is of no interest and collects.
func isBareHash(tag string) bool {
	if !hashRe.MatchString(tag) {
		return false
	}
	for i := 0; i < len(tag); i++ {
		if tag[i] >= 'a' && tag[i] <= 'f' {
			return true
		}
	}
	return !isDateLike(tag)
}

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
	// A regex edit that drops a capture group must degrade to no-match
	// (keep), never to an index panic mid-reap.
	if len(m) != 3 {
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
	// NOTE(mutants): bound-1 is equivalent — 719 still clamps
	// everything at/over 720h identically (720h is MaxTTL either
	// way); bound+1 is pinned by TestEffectiveTTLMaxBoundaryExact.
	if n > uint64(MaxTTL/unit) {
		return MaxTTL, true
	}
	return time.Duration(n) * unit, true
}

// EffectiveTTL resolves a tag to its TTL: the parsed value clamped to
// MaxTTL, the HashTTL default for bare commit hashes, or the
// DefaultTTL fallback for anything else. ok is false when the tag
// never expires (empty default), so callers treat it as keep without
// comparing durations.
func EffectiveTTL(tag string) (ttl time.Duration, ok bool) {
	ttl, matched := parseTTL(tag)
	if !matched {
		if isBareHash(tag) {
			return HashTTL, true
		}
		if DefaultTTL == 0 {
			return 0, false
		}
		return DefaultTTL, true
	}
	// NOTE: no clamp here on purpose — parseTTL saturates at
	// exactly MaxTTL (overflow and over-bound both return MaxTTL,
	// never more), so a second clamp would guard nothing.
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
