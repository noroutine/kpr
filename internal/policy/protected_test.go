package policy

import (
	"testing"
	"time"
)

// Infra-protected tag styles, sampled from
// ../infra/infra-setup/variables.yml (2026-09): the tags the homelab
// actually pins. Re-sample when infra tagging changes.
//
// Contract: no policy except keep-N may mark these. keep-N is
// retention counting — it may sweep anything, so it is deliberately
// unasserted here. Any other selector marking a protected tag means a
// policy change widened its match (usually the TTL regex) and must be
// reviewed case by case, not shipped.
//
// Each style carries the shapes that make it a trap: distro suffixes
// ending in unit letters (slim/dind), date stamps, long numeric tails,
// commit-ish tails. If this fails, a widened matcher is eating pinned
// infrastructure.
var protectedTagStyles = []struct {
	style string
	tags  []string
}{
	{"plain semver", []string{"3.24.2", "1.38.0", "13.2", "9.2", "3.10"}},
	{"v semver", []string{"v0.2.11", "v1.37.1", "v3.5.3", "v0.133"}},
	{"distro codename date", []string{"trixie-20260918-slim", "noble-20260911"}},
	{"semver distro suffix", []string{"1.27.1-trixie", "18.6-trixie", "3.2.10-alpine", "2.15.0-alpine", "35.0.1-apache", "3.10.5-core"}},
	{"number codename", []string{"25-noble"}},
	{"dashed numeric builds", []string{"26.0.2.1-26.32", "26.7.4-0", "29.8.1-dind"}},
	{"v builds", []string{"19.4.1-ce.0", "alpine-v19.4.0", "x86_64-v19.4.1"}},
	{"date versions", []string{"2026.09.0", "2026.9.3", "2026.9.4"}},
	{"word tags", []string{"stable"}},
	{"release stamps", []string{"RELEASE.2025-04-22T22-12-26Z"}},
	{"commit-ish date tails", []string{"2024.10.22-7ca5933"}},
	{"long numeric tails", []string{"12.1.20260915-010956", "0.0.20230223"}},
	{"ubi suffixes", []string{"13.4.1-base-ubi9", "v0.16.2-ubi8", "26.08-py3"}},
	{"python variants", []string{"3.3.2-python3.13", "3.8.6-python3.14"}},
	{"openssl builds", []string{"2.0.22-openssl"}},
	{"timescale builds", []string{"pg17-ts2.23"}},
}

func TestProtectedTagsSurviveAllButKeepN(t *testing.T) {
	now := time.Now()
	for _, style := range protectedTagStyles {
		for _, tag := range style.tags {
			t.Run(style.style+"/"+tag, func(t *testing.T) {
				if ttl, ok := EffectiveTTL(tag); ok {
					t.Errorf("EffectiveTTL(%q) = (%v, true): protected style parses as TTL", tag, ttl)
				}
				// Old but live: digest set, tag still in the
				// catalog. keep-N aside, no selector may mark it.
				rows := []Row{{
					Repo:     "infra/probe",
					Tag:      tag,
					Digest:   "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd",
					PushedAt: now.Add(-200 * time.Hour),
				}}
				catalog := map[string][]string{"infra/probe": {tag}}
				if due := SelectExpired(rows, now); len(due) != 0 {
					t.Errorf("SelectExpired marked protected %q: %+v", tag, due)
				}
				if due := SelectStaleUploads(rows, now); len(due) != 0 {
					t.Errorf("SelectStaleUploads marked protected %q: %+v", tag, due)
				}
				if due := SelectUntagged(rows, catalog, now); len(due) != 0 {
					t.Errorf("SelectUntagged marked protected %q: %+v", tag, due)
				}
			})
		}
	}
}
