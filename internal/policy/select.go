package policy

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"nrtn.dev/catalyst/kpr/internal/helpers/human"
)

// latestTag is spared by every collecting policy and never counts
// into keep-N: it always points at the current image, so collecting
// it deletes whatever is newest, and the next push recreates it
// anyway. SelectUntagged is the exception: it only sees tags the
// catalog already dropped, where no image is left to protect.
const latestTag = "latest"

// Tunings for the housekeeping behaviors (docs/ARCHITECTURE.md, Behaviors). They live
// here, next to the code that reads them — not in the main config.
const (
	// StaleUploadMaxAge bounds interrupted pushes: a row with no
	// digest older than this is push residue, not a retry in flight.
	StaleUploadMaxAge = 24 * time.Hour
	// UntaggedGrace is how long a manifest whose tag vanished upstream
	// is kept before collection.
	UntaggedGrace = 168 * time.Hour
	// KeepN is the default count of freshest tags a repo keeps.
	KeepN = 10
)

// Row is one tracked tag: generous at ingest (repo/tag/digest/media,
// push time, actor) so reap rarely needs a manifest fetch, plus the
// mark (Due + Reason) selectors attach. Blob content is never fetched.
type Row struct {
	Repo      string
	Tag       string
	Digest    string
	MediaType string
	PushedAt  time.Time
	Actor     string
	Due       bool
	Reason    string
}

// mark returns a due-marked copy; selectors never mutate their input
// so reap can print the plan from the same rows it marks.
func mark(r Row, reason string) Row {
	r.Due = true
	r.Reason = reason
	return r
}

// SelectTTL marks rows whose explicit TTL elapsed since push: bare
// ttl.sh numbers and suffixed tags. Bare hashes are not TTL tags —
// they go to SelectHashes.
func SelectTTL(rows []Row, now time.Time) []Row {
	var due []Row
	for _, r := range rows {
		if r.Tag == latestTag {
			continue
		}
		ttl, ok := parseTTL(r.Tag)
		if !ok || r.PushedAt.IsZero() {
			continue
		}
		if !r.PushedAt.Add(ttl).After(now) {
			due = append(due, mark(r, fmt.Sprintf("ttl:%s elapsed", human.Dur(ttl))))
		}
	}
	return due
}

// SelectHashes marks bare commit hashes past the hash default:
// pushes with no TTL encoding at all, eligible after 48h of
// implied TTL. Unknown
// age defaults keep, exactly like every other age-anchored selector.
func SelectHashes(rows []Row, now time.Time) []Row {
	var due []Row
	for _, r := range rows {
		if r.Tag == latestTag {
			continue
		}
		if !isBareHash(r.Tag) || r.PushedAt.IsZero() {
			continue
		}
		if !r.PushedAt.Add(HashTTL).After(now) {
			due = append(due, mark(r, fmt.Sprintf("ttl:%s elapsed", human.Dur(HashTTL))))
		}
	}
	return due
}

// SelectStaleUploads marks digest-less rows older than the max age:
// pushes that never completed. Unknown age defaults keep.
func SelectStaleUploads(rows []Row, now time.Time) []Row {
	var due []Row
	for _, r := range rows {
		if r.Tag == latestTag {
			continue
		}
		if r.Digest != "" || r.PushedAt.IsZero() {
			continue
		}
		if now.Sub(r.PushedAt) > StaleUploadMaxAge {
			due = append(due, mark(r, fmt.Sprintf("partial:older than %s", human.Dur(StaleUploadMaxAge))))
		}
	}
	return due
}

// SelectUntagged marks rows whose tag left the catalog past the grace
// period: tags deleted upstream leave manifests behind. A repo with no
// catalog entry (fetch failed) is skipped — absent means unknown, only
// a fetched-but-empty list means "everything gone".
func SelectUntagged(rows []Row, catalog map[string][]string, now time.Time) []Row {
	live := map[string]bool{}
	for repo, tags := range catalog {
		for _, t := range tags {
			live[repo+"\x00"+t] = true
		}
	}
	var due []Row
	for _, r := range rows {
		if _, known := catalog[r.Repo]; !known {
			continue
		}
		if live[r.Repo+"\x00"+r.Tag] {
			continue
		}
		if r.PushedAt.IsZero() || now.Sub(r.PushedAt) <= UntaggedGrace {
			continue
		}
		due = append(due, mark(r, fmt.Sprintf("untagged:past grace %s", human.Dur(UntaggedGrace))))
	}
	return due
}

// compileRes compiles exclude/include patterns; nil means no filter.
func compileRes(exprs []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, e := range exprs {
		if re, err := regexp.Compile(e); err == nil {
			out = append(out, re)
		}
	}
	return out
}

func anyMatch(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// qualified names a row registry-relative (repo:tag): the subject
// include/exclude patterns match against, so one expression scopes
// whole repos (^app:release-) or tag shapes across repos (:latest$).
func qualified(r Row) string { return r.Repo + ":" + r.Tag }

// SelectKeepN marks all but the n freshest tags per repo. Excluded
// tags (release lines, :latest) are never victims; when include is
// non-empty only matching tags participate. Catalog-only tags with no
// row (unknown age) default keep — only tracked rows are candidates.
func SelectKeepN(rows []Row, n int, include, exclude []string, now time.Time) []Row {
	_ = now
	inc, exc := compileRes(include), compileRes(exclude)
	byRepo := map[string][]Row{}
	for _, r := range rows {
		// latest neither counts into the n nor takes a fall for
		// being oldest: the repo keeps n plus latest.
		if r.Tag == latestTag {
			continue
		}
		if anyMatch(exc, qualified(r)) {
			continue
		}
		if len(inc) > 0 && !anyMatch(inc, qualified(r)) {
			continue
		}
		byRepo[r.Repo] = append(byRepo[r.Repo], r)
	}
	var due []Row
	for _, group := range byRepo {
		sort.Slice(group, func(i, j int) bool {
			return group[i].PushedAt.After(group[j].PushedAt)
		})
		for i, r := range group {
			if i >= n {
				due = append(due, mark(r, fmt.Sprintf("keep-n:exceeds %d", n)))
			}
		}
	}
	return due
}
