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

// Exactly-at-max-age is still a retry in flight; one nanosecond past
// is residue. If this fails, the boundary wobbles and uploads get
// flagged while still retrying (or residue lingers a tick too long).
func TestSelectStaleUploadsBoundaryExact(t *testing.T) {
	at := []Row{{Repo: "app", Tag: "edge", PushedAt: sliceNow.Add(-StaleUploadMaxAge)}}
	if got := SelectStaleUploads(at, sliceNow); len(got) != 0 {
		t.Errorf("selected %v at exactly max age, want kept", got)
	}
	past := []Row{{Repo: "app", Tag: "edge", PushedAt: sliceNow.Add(-StaleUploadMaxAge - time.Nanosecond)}}
	if got := SelectStaleUploads(past, sliceNow); len(got) != 1 {
		t.Errorf("selected %v past max age, want [edge]", got)
	}
}

// Exactly-at-grace is still protected; one nanosecond past is dead
// weight. If this fails, manifests vanish a tick early (or linger a
// tick late) around the grace boundary.
func TestSelectUntaggedBoundaryExact(t *testing.T) {
	catalog := map[string][]string{"app": {"other"}}
	at := []Row{mkrow("app", "gone", UntaggedGrace)}
	if got := SelectUntagged(at, catalog, sliceNow); len(got) != 0 {
		t.Errorf("selected %v at exactly grace, want kept", got)
	}
	past := []Row{mkrow("app", "gone", UntaggedGrace+time.Nanosecond)}
	if got := SelectUntagged(past, catalog, sliceNow); len(got) != 1 {
		t.Errorf("selected %v past grace, want [gone]", got)
	}
}

// A repo with no catalog at all (fetch failed) is skipped, never
// treated as empty: an unreadable catalog must not read as "every row
// untagged". If this fails, a registry blip mass-marks old rows.
func TestSelectUntaggedSkipsUnknownRepo(t *testing.T) {
	rows := []Row{mkrow("ghost", "v1", 200*24*time.Hour)}
	if got := SelectUntagged(rows, map[string][]string{}, sliceNow); len(got) != 0 {
		t.Errorf("selected %v without a catalog, want none (skip, not empty)", got)
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

// A non-empty include scopes candidacy: only matching rows vie,
// the rest keep regardless of age. An empty include means everybody
// vies. If this fails, scoping a run to one repo still reaps the
// others.
func TestSelectKeepNIncludeScopesCandidacy(t *testing.T) {
	rows := []Row{
		mkrow("app", "v1", 30*24*time.Hour),
		mkrow("other", "v1", 300*24*time.Hour),
	}
	got := SelectKeepN(rows, 0, []string{"^app:"}, nil, sliceNow)
	if len(got) != 1 || got[0].Repo != "app" {
		t.Fatalf("selected %v, want [app:v1] only (other: out of scope keeps)", got)
	}
}

// Include/exclude patterns match the qualified name repo:tag (registry
// stripped), so one flag scopes whole repos: ^app:release- spares app
// releases while other:release-1 still vies. A tag-anchored pattern
// (^v1$) matches nothing — the repo prefix is part of the subject. If
// this fails, excludes silently lost their repo scope.
func TestSelectKeepNMatchesQualifiedNames(t *testing.T) {
	rows := []Row{
		mkrow("app", "release-1", 100*24*time.Hour),
		mkrow("app", "v1", 3*time.Hour),
		mkrow("other", "release-1", 100*24*time.Hour),
	}
	got := SelectKeepN(rows, 0, nil, []string{"^app:release-"}, sliceNow)
	names := map[string]bool{}
	for _, r := range got {
		names[r.Repo+":"+r.Tag] = true
	}
	if len(got) != 2 || !names["app:v1"] || !names["other:release-1"] {
		t.Errorf("selected %v, want [app:v1 other:release-1]", got)
	}
	anchored := SelectKeepN(rows, 0, nil, []string{"^v1$"}, sliceNow)
	if len(anchored) != 3 {
		t.Errorf("tag-anchored exclude spared %d rows, want 0 spared (subject is repo:tag)", 3-len(anchored))
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
