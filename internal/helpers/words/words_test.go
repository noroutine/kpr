package words_test

import (
	"testing"

	"nrtn.dev/catalyst/kpr/internal/helpers/words"
)

// Every command counts nouns the same way: 1 repo, 2 repos, 0
// repos. If this fails, one command's counts read differently
// from the rest.
func TestPluralCountsNouns(t *testing.T) {
	for _, tc := range []struct {
		n         int
		one, many string
		want      string
	}{
		{1, "repo", "repos", "1 repo"},
		{2, "repo", "repos", "2 repos"},
		{0, "repo", "repos", "0 repos"},
		{1, "tag", "tags", "1 tag"},
	} {
		if got := words.Plural(tc.n, tc.one, tc.many); got != tc.want {
			t.Errorf("Plural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// Deltas read signed with singular nouns at ±1: +1 repo, -1 tag,
// +0 sentinels. If this fails, a delta line miscounts its nouns.
func TestSignedPluralSignsDeltas(t *testing.T) {
	for _, tc := range []struct {
		n         int
		one, many string
		want      string
	}{
		{1, "repo", "repos", "+1 repo"},
		{-1, "tag", "tags", "-1 tag"},
		{0, "sentinel", "sentinels", "+0 sentinels"},
		{3, "repo", "repos", "+3 repos"},
	} {
		if got := words.SignedPlural(tc.n, tc.one, tc.many); got != tc.want {
			t.Errorf("SignedPlural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
