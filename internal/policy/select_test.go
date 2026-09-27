package policy

import (
	"testing"
	"time"
)

var sliceNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func mkrow(repo, tag string, age time.Duration) Row {
	return Row{Repo: repo, Tag: tag, Digest: "sha256:abc", PushedAt: sliceNow.Add(-age)}
}

// An expired ephemeral tag must come back marked due with its reason
// attached, while a fresh one and a :latest stay unmarked. If this
// fails, reap either misses TTL rows or marks rows it shouldn't, and
// the console plan lies about what the next sweep would delete.
func TestSelectExpiredMarksOnlyDue(t *testing.T) {
	rows := []Row{
		mkrow("scratch", "10m", 11*time.Minute),
		mkrow("scratch", "10m", 9*time.Minute),
		mkrow("app", "latest", 365*24*time.Hour),
	}
	got := SelectExpired(rows, sliceNow)
	if len(got) != 1 {
		t.Fatalf("selected %d rows, want 1", len(got))
	}
	if !got[0].Due || got[0].Reason == "" {
		t.Errorf("selected row not marked due with reason: %+v", got[0])
	}
	if got[0].Repo != "scratch" || got[0].Tag != "10m" {
		t.Errorf("selected %+v, want scratch:10m", got[0])
	}
}

// A push that never completed leaves a row with no digest behind;
// past the colocated max age it is interrupted-push residue and must
// be marked. A digest-less row still inside the window stays. If this
// fails, push-interrupt storms accumulate forever (or fresh uploads
// get flagged while still retrying).
func TestSelectStaleUploadsNeedsMissingDigestAndAge(t *testing.T) {
	rows := []Row{
		{Repo: "app", Tag: "wip", PushedAt: sliceNow.Add(-25 * time.Hour)},
		{Repo: "app", Tag: "retrying", PushedAt: sliceNow.Add(-time.Hour)},
		mkrow("app", "v1", 30*24*time.Hour),
	}
	got := SelectStaleUploads(rows, sliceNow)
	if len(got) != 1 || got[0].Tag != "wip" {
		t.Fatalf("selected %v, want [wip]", got)
	}
	if !got[0].Due || got[0].Reason == "" {
		t.Errorf("stale upload not marked due with reason: %+v", got[0])
	}
}

// Tags deleted upstream leave manifest rows behind; past the grace
// period they are dead weight and must be marked. A row whose tag is
// still in the catalog stays even when ancient. If this fails, reap
// either leaks deleted-tag manifests or deletes live tags' manifests.
func TestSelectUntaggedNeedsCatalogAbsenceAndGrace(t *testing.T) {
	rows := []Row{
		mkrow("app", "gone", 200*24*time.Hour),
		mkrow("app", "recently-gone", 24*time.Hour),
		mkrow("app", "live", 200*24*time.Hour),
	}
	catalog := map[string][]string{"app": {"live", "recently-gone"}}
	got := SelectUntagged(rows, catalog, sliceNow)
	if len(got) != 1 || got[0].Tag != "gone" {
		t.Fatalf("selected %v, want [gone]", got)
	}
}

// A repo keeping its N freshest tags marks everything older, with the
// reason naming the tuning. Fresh-enough tags stay regardless of age.
// If this fails, long-lived repos grow unbounded (or the policy eats
// the tags it promised to keep).
func TestSelectKeepNMarksBeyondFreshest(t *testing.T) {
	rows := []Row{
		mkrow("app", "v3", time.Hour),
		mkrow("app", "v2", 2*time.Hour),
		mkrow("app", "v1", 3*time.Hour),
	}
	got := SelectKeepN(rows, 2, nil, nil, sliceNow)
	if len(got) != 1 || got[0].Tag != "v1" {
		t.Fatalf("selected %v, want [v1]", got)
	}
	if !got[0].Due || got[0].Reason == "" {
		t.Errorf("keep-N victim not marked due with reason: %+v", got[0])
	}
}

// Excluded tags are never keep-N victims (release tags, :latest),
// while included ones are. If this fails, the safety hatch operators
// use to protect blessed tags does nothing.
func TestSelectKeepNHonorsIncludeExclude(t *testing.T) {
	rows := []Row{
		mkrow("app", "release-1", 100*24*time.Hour),
		mkrow("app", "v1", 3*time.Hour),
		mkrow("app", "v2", 2*time.Hour),
		mkrow("app", "v3", time.Hour),
	}
	got := SelectKeepN(rows, 1, nil, []string{"release-.*", "latest"}, sliceNow)
	for _, r := range got {
		if r.Tag == "release-1" {
			t.Errorf("excluded tag selected: %+v", r)
		}
	}
	if len(got) != 2 {
		t.Fatalf("selected %v, want [v1 v2] (release-1 excluded, v3 kept)", got)
	}
}

// Selectors must not mutate their input: reap prints the plan from the
// same rows it marks, so an in-place mark would corrupt the unmarked
// view. If this fails, dry-run output and armed-run behavior diverge.
func TestSelectorsDoNotMutateInput(t *testing.T) {
	rows := []Row{mkrow("scratch", "10m", time.Hour)}
	_ = SelectExpired(rows, sliceNow)
	_ = SelectStaleUploads(rows, sliceNow)
	_ = SelectKeepN(rows, 0, nil, nil, sliceNow)
	if rows[0].Due || rows[0].Reason != "" {
		t.Errorf("input mutated: %+v", rows[0])
	}
}
