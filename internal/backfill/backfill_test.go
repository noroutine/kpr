package backfill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stubStatus is the typed registry failure the client reports:
// backfill classifies 404s by count, everything else refuses.
type stubStatus struct{ code int }

func (s *stubStatus) Error() string   { return fmt.Sprintf("registry status %d", s.code) }
func (s *stubStatus) StatusCode() int { return s.code }

// fileAPI serves generations off a TempDir volume exactly like the
// shared mount: link file to manifest digest, blob under the
// content path. Same shape as gc's seam stub.
type fileAPI struct{ root string }

func (f fileAPI) blob(digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	return os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2", "blobs", "sha256", hex[:2], hex, "data"))
}

func (f fileAPI) GetManifest(_ context.Context, repo, tag string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return nil, err
	}
	return f.blob(strings.TrimSpace(string(raw)))
}

func (f fileAPI) GetBlob(_ context.Context, repo, digest string) ([]byte, error) {
	return f.blob(digest)
}

// stubRegistry is the programmable catalog: repos, per-repo tags,
// per-tag digests, each with its own failure.
type stubRegistry struct {
	repos    []string
	reposErr error
	tags     map[string][]string
	tagsErr  map[string]error
	digests  map[string]string
	digErr   map[string]error
}

func (s *stubRegistry) CatalogAll(context.Context) ([]string, error) {
	if s.reposErr != nil {
		return nil, s.reposErr
	}
	return s.repos, nil
}

func (s *stubRegistry) Catalog(_ context.Context, repo string) ([]string, error) {
	if err, ok := s.tagsErr[repo]; ok {
		return nil, err
	}
	return s.tags[repo], nil
}

func (s *stubRegistry) ManifestDigest(_ context.Context, repo, tag string) (string, string, error) {
	k := repo + "\x00" + tag
	if err, ok := s.digErr[k]; ok {
		return "", "", err
	}
	return s.digests[k], "application/vnd.oci.image.manifest.v1+json", nil
}

// stagePaired writes a served generation and pairs the store to it,
// tracking the generation like a proven run would. Returns the root
// (volume layout), the mem store (rows, identity, lock), the served
// gen, and the store identity.
func stagePaired(t *testing.T) (root string, s *store.MemStore, gen, id string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()
	s = store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock staging store: %v", err)
	}
	var gerr, ierr error
	gen, gerr = sentinel.NewGen()
	if gerr != nil {
		t.Fatalf("mint gen: %v", gerr)
	}
	id, ierr = sentinel.NewGen()
	if ierr != nil {
		t.Fatalf("mint id: %v", ierr)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: id, TS: now}); err != nil {
		t.Fatalf("stage generation: %v", err)
	}
	if err := s.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: gen}); err != nil {
		t.Fatalf("pair store: %v", err)
	}
	if err := s.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: gen, PushedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("track generation: %v", err)
	}
	return root, s, gen, id
}

// stageTagDir lays a tag link with a real mtime under root: the
// layout tagMtime stats. Returns the link's mtime.
func stageTagDir(t *testing.T, root, repo, tag string) time.Time {
	t.Helper()
	dir := filepath.Join(root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir tag dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "link"), []byte("sha256:abc"), 0o644); err != nil {
		t.Fatalf("write link: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "link"))
	if err != nil {
		t.Fatalf("stat link: %v", err)
	}
	return fi.ModTime().UTC()
}

// Fossil sentinels adopt while the floater never becomes a row:
// a rerun skips both by verdict. If this fails, the store never
// learns the generations pushed while kpr was unwired — or a
// `latest` row poisons the newest-generation verdict.
func TestBackfillAdoptsSentinelFossils(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	repo := "noroutine/kpr-shadow"
	old := time.Now().UTC().Add(-48 * time.Hour)
	stageTagDir(t, root, repo, "fossil")
	retime(t, root, repo, "fossil", old)
	stageTagDir(t, root, repo, sentinel.Tag)
	reg := &stubRegistry{
		repos: []string{repo},
		tags:  map[string][]string{repo: {"fossil", sentinel.Tag}},
		digests: map[string]string{
			repo + "\x00fossil":          "sha256:fossil",
			repo + "\x00" + sentinel.Tag: "sha256:floater",
		},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 1 || sum.Skipped != 1 {
		t.Fatalf("sum = %+v, want 1 recorded (fossil), 1 skipped (floater)", sum)
	}
	rows, _ := s.All(ctx)
	byTag := map[string]policy.Row{}
	for _, r := range rows {
		if r.Repo == repo {
			byTag[r.Tag] = r
		}
	}
	fossil, ok := byTag["fossil"]
	if !ok {
		t.Fatal("no fossil row recorded")
	}
	if fossil.Actor != ActorBackfill || fossil.Due || !fossil.PushedAt.Equal(old) {
		t.Errorf("fossil = %+v, want kpr-backfill + not-due + old link mtime", fossil)
	}
	if _, ok := byTag[sentinel.Tag]; ok {
		t.Fatal("floater row recorded — latest is a pointer, not inventory")
	}
	if sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil); err != nil {
		t.Fatalf("rerun: %v", err)
	} else if sum.Recorded != 0 || sum.Skipped != 2 {
		t.Fatalf("rerun = %+v, want 0 recorded, 2 skipped", sum)
	}
}

// A floater ahead of a fossil never ends the repo: the floater
// skip is per tag, the walk continues. If this fails, imports
// stop at the first untracked sentinel tag.
func TestBackfillFloaterFirstStillAdoptsFossil(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	repo := "noroutine/kpr-shadow"
	old := time.Now().UTC().Add(-48 * time.Hour)
	stageTagDir(t, root, repo, "fossil")
	retime(t, root, repo, "fossil", old)
	stageTagDir(t, root, repo, sentinel.Tag)
	reg := &stubRegistry{
		repos: []string{repo},
		tags:  map[string][]string{repo: {sentinel.Tag, "fossil"}},
		digests: map[string]string{
			repo + "\x00fossil":          "sha256:fossil",
			repo + "\x00" + sentinel.Tag: "sha256:floater",
		},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 1 || sum.Skipped != 1 {
		t.Fatalf("sum = %+v, want 1 recorded (fossil), 1 skipped (floater)", sum)
	}
}

// A fossil rewritten past the live generation clamps at the
// floater's push: nothing backfilled may outrank latest in
// keep-N ordering. If this fails, a touched fossil sorts as
// fresher than the generation it predates.
func TestBackfillClampsSentinelNewerThanLatest(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	repo := "noroutine/kpr-shadow"
	old := time.Now().UTC().Add(-48 * time.Hour)
	stageTagDir(t, root, repo, sentinel.Tag)
	retime(t, root, repo, sentinel.Tag, old)
	stageTagDir(t, root, repo, "fossil")
	reg := &stubRegistry{
		repos: []string{repo},
		tags:  map[string][]string{repo: {"fossil", sentinel.Tag}},
		digests: map[string]string{
			repo + "\x00fossil":          "sha256:fossil",
			repo + "\x00" + sentinel.Tag: "sha256:floater",
		},
	}
	var out strings.Builder
	if _, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, _ := s.All(ctx)
	found := false
	for _, r := range rows {
		if r.Repo != repo || r.Tag != "fossil" {
			continue
		}
		found = true
		if r.PushedAt.After(old) {
			t.Fatalf("fossil PushedAt = %v, want clamped at latest %v", r.PushedAt, old)
		}
	}
	if !found {
		t.Fatal("no fossil row recorded")
	}
}

// retime backdates a staged tag link: stageTagDir stamps now,
// fossils need age.
func retime(t *testing.T, root, repo, tag string, at time.Time) {
	t.Helper()
	link := filepath.Join(root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current", "link")
	if err := os.Chtimes(link, at, at); err != nil {
		t.Fatalf("retime link: %v", err)
	}
}

// Every verdict streams a line: record lines alone leave skips
// invisible, and a 17k-skip run greps empty. If this fails, the
// per-tag log omits an outcome the counters count.
func TestBackfillLogsEveryVerdict(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	if err := s.Record(ctx, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:old", Actor: "kpr-receiver"}); err != nil {
		t.Fatalf("pre-track: %v", err)
	}
	stageTagDir(t, root, "app", "v1")
	stageTagDir(t, root, "app", "v2")
	stageTagDir(t, root, "noroutine/kpr-shadow", "fossil")
	stageTagDir(t, root, "noroutine/kpr-shadow", "latest")
	reg := &stubRegistry{
		repos: []string{"app", "noroutine/kpr-shadow"},
		tags: map[string][]string{
			"app":                  {"v1", "v2"},
			"noroutine/kpr-shadow": {"fossil", "latest"},
		},
		digests: map[string]string{
			"app\x00v1": "sha256:old", "app\x00v2": "sha256:abc",
			"noroutine/kpr-shadow\x00fossil": "sha256:f",
			"noroutine/kpr-shadow\x00latest": "sha256:l",
		},
	}
	var dry strings.Builder
	if _, err := Run(ctx, io.Discard, fileAPI{root}, reg, s, s, s, s, root, Options{DryRun: true, Log: &dry}, nil); err != nil {
		t.Fatalf("dry Run: %v", err)
	}
	for _, want := range []string{
		"would record app:v2 sha256:abc",
		"would skip app:v1 (tracked)",
		"would record noroutine/kpr-shadow:fossil sha256:f",
		"would skip noroutine/kpr-shadow:latest (floater)",
	} {
		if !strings.Contains(dry.String(), want) {
			t.Errorf("dry stream missing %q:\n%s", want, dry.String())
		}
	}
	var armed strings.Builder
	if _, err := Run(ctx, io.Discard, fileAPI{root}, reg, s, s, s, s, root, Options{Log: &armed}, nil); err != nil {
		t.Fatalf("armed Run: %v", err)
	}
	for _, want := range []string{
		"recorded app:v2 sha256:abc",
		"skipped app:v1 (tracked)",
		"recorded noroutine/kpr-shadow:fossil sha256:f",
		"skipped noroutine/kpr-shadow:latest (floater)",
	} {
		if !strings.Contains(armed.String(), want) {
			t.Errorf("armed stream missing %q:\n%s", want, armed.String())
		}
	}
}

func accept(t *testing.T) proof.AcceptedRisk {
	t.Helper()
	return proof.Force(proof.Arm(true, false), true)
}

// An absent tag records with its link mtime, signed kpr-backfill,
// not due: the row a re-push would have made, minus the push. If
// this fails, pre-kpr tags stay invisible to every selector.
func TestBackfillRecordsAbsentWithMtimes(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	mtime := stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Recorded: 1, Tracked: 0, Sentinels: 1, Repos: 1, Tags: 1}) {
		t.Errorf("sum = %+v, want recorded, 0 adoptable, 1 sentinel, scan of 1 repo, 1 tag", sum)
	}
	rows, _ := s.All(ctx)
	if len(rows) != 2 {
		t.Fatalf("tracked rows = %d, want gen + app:v1", len(rows))
	}
	var got *policy.Row
	for i, r := range rows {
		if r.Repo == "app" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("no app:v1 row recorded")
	}
	if got.Digest != "sha256:abc" || got.Actor != ActorBackfill || got.Due || !got.PushedAt.Equal(mtime) {
		t.Errorf("row = %+v, want digest + kpr-backfill + not-due + link mtime", got)
	}
}

// Progress reports every verdict as it lands: the caller renders
// live counters from it. If this fails, counters lag the run.
func TestBackfillProgressReportsVerdicts(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	stageTagDir(t, root, "app", "v2")
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1", "v2"}},
		digests: map[string]string{"app\x00v1": "sha256:abc", "app\x00v2": "sha256:def"},
	}
	var last Summary
	n := 0
	opts := Options{DryRun: true, Log: io.Discard, Progress: func(sum Summary) {
		last, n = sum, n+1
	}}
	if _, err := Run(ctx, io.Discard, fileAPI{root}, reg, s, s, s, s, root, opts, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n == 0 {
		t.Fatal("no progress reported")
	}
	if last.Recorded != 2 || last.Skipped != 0 || last.Failed != 0 {
		t.Errorf("last progress = %+v, want 2 recorded", last)
	}
}

// Progress reports the scan as it lands: the tracked baseline,
// then repos and tags accumulating per listed repo. If this
// fails, the catalog line never moves.
func TestBackfillProgressReportsScan(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app1", "v1")
	stageTagDir(t, root, "app2", "v1")
	stageTagDir(t, root, "app2", "v2")
	reg := &stubRegistry{
		repos: []string{"app1", "app2"},
		tags: map[string][]string{
			"app1": {"v1"},
			"app2": {"v1", "v2"},
		},
		digests: map[string]string{
			"app1\x00v1": "sha256:aaa",
			"app2\x00v1": "sha256:bbb",
			"app2\x00v2": "sha256:ccc",
		},
	}
	var snaps []Summary
	opts := Options{DryRun: true, Log: io.Discard, Progress: func(sum Summary) {
		snaps = append(snaps, sum)
	}}
	if _, err := Run(ctx, io.Discard, fileAPI{root}, reg, s, s, s, s, root, opts, nil); err != nil {
		t.Fatalf("Run: %v, want scan", err)
	}
	if len(snaps) == 0 {
		t.Fatal("no progress reported")
	}
	if snaps[0].Tracked != 0 || snaps[0].Sentinels != 1 {
		t.Errorf("baseline = %+v, want 0 adoptable, 1 sentinel generation row", snaps[0])
	}
	var repos, tags int
	for _, sn := range snaps {
		if sn.Tracked != 0 || sn.Sentinels != 1 {
			t.Errorf("snapshot = %+v, want tracked baseline carried", sn)
		}
		repos, tags = max(repos, sn.Repos), max(tags, sn.Tags)
	}
	if repos != 2 || tags != 3 {
		t.Errorf("scan peaks at %d repos, %d tags, want 2 and 3", repos, tags)
	}
}

// Sentinel rows count apart from the tracked baseline: the
// generation row is machinery, and `store ls` hides it too — the
// split keeps both views agreeing. If this fails, sentinel rows
// inflate the adoptable count.
func TestBackfillCountsSentinelsSeparately(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	if err := s.Record(ctx, policy.Row{Repo: "app", Tag: "v9", PushedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("record app row: %v", err)
	}
	stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	sum, err := Run(ctx, io.Discard, fileAPI{root}, reg, s, s, s, s, root, Options{DryRun: true, Log: io.Discard}, nil)
	if err != nil {
		t.Fatalf("Run: %v, want preview", err)
	}
	if sum.Tracked != 1 || sum.Sentinels != 1 {
		t.Errorf("sum = %+v, want 1 adoptable row apart from 1 sentinel", sum)
	}
}

// The run prints no summary itself: warnings own the writer, the
// caller renders the counters from the returned Summary. If this
// fails, the display double-prints.
func TestBackfillRunPrintsNoSummary(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	opts := Options{DryRun: true, Log: io.Discard}
	if _, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, opts, nil); err != nil {
		t.Fatalf("Run: %v, want preview", err)
	}
	if out.Len() != 0 {
		t.Errorf("run wrote %q, want warnings only (none here)", out.String())
	}
}

// A preview prints what it would stamp to the log sink and records
// nothing: the operator reviews before arming. No sink, no stream
// — the per-tag lines need an explicit Log. If this fails, dry
// runs lie or write.
func TestBackfillDryRunPrintsWithoutRecording(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, io.Discard, fileAPI{root}, reg, s, s, s, s, root, Options{DryRun: true, Log: &out}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "would record app:v1") {
		t.Errorf("preview prints no would-record:\n%s", out.String())
	}
	rows, _ := s.All(ctx)
	if len(rows) != 1 {
		t.Errorf("dry run recorded %d rows, want none added", len(rows)-1)
	}
	if sum.Recorded != 1 {
		t.Errorf("sum = %+v, want the would-record counted", sum)
	}
}

// Tracked tags are no-ops: absence-only means a rerun over
// receiver-known rows changes nothing. If this fails, backfill
// clobbers live tracking.
func TestBackfillSkipsTracked(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	if err := s.Record(ctx, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:old", Actor: "kpr-receiver"}); err != nil {
		t.Fatalf("pre-track: %v", err)
	}
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 0 || sum.Skipped != 1 {
		t.Errorf("sum = %+v, want {Skipped:1}", sum)
	}
	rows, _ := s.All(ctx)
	for _, r := range rows {
		if r.Repo == "app" && r.Digest != "sha256:old" {
			t.Errorf("tracked row clobbered: %+v", r)
		}
	}
}

// A tracked generation skips while its untracked sibling adopts:
// the skip is per row, never per repo. If this fails, reruns
// re-record the live generation.
func TestBackfillSkipsTrackedSentinelGen(t *testing.T) {
	ctx := context.Background()
	root, s, gen, _ := stagePaired(t)
	stageTagDir(t, root, sentinel.Repo, gen)
	stageTagDir(t, root, sentinel.Repo, "fossil")
	reg := &stubRegistry{
		repos: []string{sentinel.Repo},
		tags:  map[string][]string{sentinel.Repo: {gen, "fossil"}},
		digests: map[string]string{
			sentinel.Repo + "\x00" + gen: "sha256:new",
			sentinel.Repo + "\x00fossil": "sha256:fossil",
		},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 1 || sum.Skipped != 1 {
		t.Errorf("sum = %+v, want 1 recorded (fossil), 1 skipped (live gen)", sum)
	}
	rows, _ := s.All(ctx)
	for _, r := range rows {
		if r.Repo == sentinel.Repo && r.Tag == gen && r.Digest != "" {
			t.Errorf("live generation clobbered: %+v", r)
		}
	}
}

// A glob matching nothing refuses, like exact names in plan add:
// a typo'd scope must not read as an empty registry. If this
// fails, `store backfill typo-*` reports a clean zero.
func TestBackfillUnknownGlobRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{repos: []string{"app"}}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{RepoGlob: "nope-*"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no catalog repository matches") {
		t.Errorf("err = %v, want the no-match refusal", err)
	}
}

// A glob matching a repo adopts through it: the filter narrows,
// never blinds. If this fails, scoped backfills record nothing.
func TestBackfillMatchingGlobAdopts(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	stageTagDir(t, root, "other", "v1")
	reg := &stubRegistry{
		repos: []string{"app", "other"},
		tags:  map[string][]string{"app": {"v1"}, "other": {"v1"}},
		digests: map[string]string{
			"app\x00v1":   "sha256:abc",
			"other\x00v1": "sha256:def",
		},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{RepoGlob: "app*"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 1 {
		t.Errorf("sum = %+v, want 1 recorded (app only)", sum)
	}
	rows, _ := s.All(ctx)
	for _, r := range rows {
		if r.Repo == "other" {
			t.Errorf("out-of-scope row recorded: %+v", r)
		}
	}
}

// A 404 on the digest skips by count and warns, never refuses;
// a 404 on the whole repo is a husk — tagless, nothing to adopt —
// counted apart, never warned per repo. If this fails, a tag
// deleted between catalog and digest aborts the whole import, or
// husks spam a warning per repo.
func TestBackfillMidRun404Skips(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos: []string{"app", "gone", "bare"},
		tags: map[string][]string{
			"app":  {"v1", "rip"},
			"bare": {},
		},
		digests: map[string]string{
			"app\x00v1": "sha256:abc",
		},
		digErr: map[string]error{
			"app\x00rip": &stubStatus{404},
		},
		tagsErr: map[string]error{
			"gone": &stubStatus{404},
		},
	}
	var out strings.Builder
	var logged strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{Log: &logged}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 1 || sum.Failed != 1 || sum.Husks != 2 {
		t.Errorf("sum = %+v, want {Recorded:1 Failed:1 Husks:2}", sum)
	}
	if strings.Contains(out.String(), "vanished mid-run") {
		t.Errorf("output warns per husk:\n%s", out.String())
	}
	for _, want := range []string{"skipped gone (husk: no tags)", "skipped bare (husk: no tags)"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log names no %q:\n%s", want, logged.String())
		}
	}
}

// Two unreadable digests both count: the warning is a courtesy,
// never the verdict — a second failure must not hide behind the
// first warning. If this fails, mid-run rot undercounts.
func TestBackfillTwoDigestFailuresBothCount(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{
		repos: []string{"app"},
		tags:  map[string][]string{"app": {"v1", "v2"}},
		digErr: map[string]error{
			"app\x00v1": errors.New("rot"),
			"app\x00v2": errors.New("rot"),
		},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Failed != 2 {
		t.Errorf("sum = %+v, want {Failed:2}", sum)
	}
}

// A digest backfill cannot stamp without storage: a tag whose link
// never landed skips, never a zero-time row. If this fails,
// timeless rows enter the selectors.
func TestBackfillMissingLinkSkips(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 0 || sum.Failed != 1 {
		t.Errorf("sum = %+v, want {Failed:1}", sum)
	}
}

// A locked store refuses naming the ceremony, before any catalog
// read: backfill under a fence would bless rows the fence
// invalidates. If this fails, locked runs enumerate anyway.
func TestBackfillLockedRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	if err := s.SetUnlocked(ctx, false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	reg := &stubRegistry{}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if !errors.Is(err, proof.ErrLocked) {
		t.Errorf("err = %v, want the locked refusal", err)
	}
}

// A stranger's store refuses before enumerating: backfill never
// adopts by recording. If this fails, pointing at the wrong
// volume imports someone else's tags.
func TestBackfillForeignRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	if err := s.SetIdentity(ctx, store.Identity{ID: "someone-else"}); err != nil {
		t.Fatalf("re-pair: %v", err)
	}
	reg := &stubRegistry{}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "foreign lineage") {
		t.Errorf("err = %v, want the foreign refusal", err)
	}
}

// A restored generation refuses armed unless rollback-accepted,
// warns through a preview: backfill deletes nothing, but its rows
// could resurrect blobs keep-N already ate. If this fails, a
// rollback imports silently on a live run.
func TestBackfillRollbackNeedsAcceptArmed(t *testing.T) {
	ctx := context.Background()
	root, s, _, id := stagePaired(t)
	next, nerr := sentinel.NewGen()
	if nerr != nil {
		t.Fatalf("mint next gen: %v", nerr)
	}
	// A true rollback: the store tracks newer than served, and the
	// baseline is neither — the served gen is not adopted.
	if err := s.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: next}); err != nil {
		t.Fatalf("re-baseline: %v", err)
	}
	if err := s.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: next, PushedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("track newer: %v", err)
	}
	reg := &stubRegistry{repos: []string{}}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "--accept-rollback") {
		t.Errorf("err = %v, want the rollback refusal naming its flag", err)
	}

	var warn strings.Builder
	sum, err := Run(ctx, &warn, fileAPI{root}, reg, s, s, s, s, root, Options{}, accept(t))
	if err != nil {
		t.Fatalf("accepted Run: %v", err)
	}
	if !strings.Contains(warn.String(), "Warning:") {
		t.Errorf("accepted run warns nothing:\n%s", warn.String())
	}
	if sum.Recorded != 0 {
		t.Errorf("sum = %+v, want nothing to record", sum)
	}

	var preview strings.Builder
	if _, err := Run(ctx, &preview, fileAPI{root}, reg, s, s, s, s, root, Options{DryRun: true}, nil); err != nil {
		t.Errorf("dry run over rollback refused: %v", err)
	}
}

// An unreachable catalog refuses the whole run before any row:
// backfill against a dead registry must not report a clean zero.
// If this fails, outages read as empty registries.
func TestBackfillUnreachableCatalogRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{reposErr: fmt.Errorf("connection refused")}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "backfill enumeration") {
		t.Errorf("err = %v, want the enumeration refusal", err)
	}
}

// An unreadable lock marker refuses distinctly from a locked one:
// the prover cannot tell locked from broken, so the run names the
// read failure instead of the ceremony. If this fails, storage
// trouble reads as "go unlock".
func TestBackfillUnreadableLockRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, lockFail{err: errors.New("i/o")}, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "store lock unreadable") {
		t.Errorf("err = %v, want the unreadable-lock refusal", err)
	}
}

type lockFail struct{ err error }

func (l lockFail) IsUnlocked(context.Context) (bool, error) { return false, l.err }

// Unreadable tracked state refuses before enumeration: adopting
// over rows that cannot be read would double-track or collide.
// If this fails, a sick store imports blind.
func TestBackfillUnreadableRowsRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, rowsFail{err: errors.New("i/o")}, s, s, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "tracked state unreadable") {
		t.Errorf("err = %v, want the unreadable-rows refusal", err)
	}
}

type rowsFail struct{ err error }

func (r rowsFail) All(context.Context) ([]policy.Row, error) { return nil, r.err }

// Unreadable lineage refuses before enumeration: the run cannot
// judge foreign against served without the pairing. If this
// fails, identity trouble imports as a stranger's tags.
func TestBackfillUnreadableIdentityRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, idsFail{err: errors.New("i/o")}, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "lineage unreadable") {
		t.Errorf("err = %v, want the unreadable-lineage refusal", err)
	}
}

type idsFail struct{ err error }

func (f idsFail) GetIdentity(context.Context) (store.Identity, error) {
	return store.Identity{}, f.err
}

func (f idsFail) SetIdentity(context.Context, store.Identity) error { return f.err }

// A non-404 tag listing failure refuses the run: only 404 reads
// tagless, anything else is an outage mid-enumeration. If this
// fails, a 500 on one repo aborts the import as husks.
func TestBackfillTagsUnreachableRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{
		repos:   []string{"app"},
		tagsErr: map[string]error{"app": &stubStatus{code: 500}},
	}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "backfill tags for app") {
		t.Errorf("err = %v, want the tags refusal", err)
	}
}

// The log stream is load-bearing: husk lines, skip lines, and the
// stale warning all fail the run when the writer does, so a dead
// pipe never reads as a clean pass. If this fails, broken output
// reports success.
func TestBackfillLogWriteFailsRun(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{repos: []string{"bare"}, tags: map[string][]string{"bare": {}}}
	var out strings.Builder
	if _, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{Log: errWriter{}}, nil); err == nil {
		t.Error("husk over dead log succeeded, want the write failure")
	}
}

// errWriter fails every write: the dead pipe behind warning and
// verdict streams.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("pipe dead") }

// The stale warning fails the run when the pipe does: accepted
// staleness still narrates, and narration is load-bearing. If this
// fails, a dead pipe swallows the only record of running behind.
func TestBackfillStaleWarnWriteFails(t *testing.T) {
	ctx := context.Background()
	root, s, _, id := stagePaired(t)
	next, nerr := sentinel.NewGen()
	if nerr != nil {
		t.Fatalf("mint next gen: %v", nerr)
	}
	if err := s.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: next}); err != nil {
		t.Fatalf("re-baseline: %v", err)
	}
	if err := s.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: next, PushedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("track newer: %v", err)
	}
	reg := &stubRegistry{repos: []string{}}
	if _, err := Run(ctx, errWriter{}, fileAPI{root}, reg, s, s, s, s, root, Options{}, accept(t)); err == nil {
		t.Error("stale warning over dead pipe succeeded, want the write failure")
	}
}

// An unreadable digest warns and skips by count — unless the
// warning itself cannot be written, which fails the run. If this
// fails, digest trouble during import reports a clean skip.
func TestBackfillDigestWarnWriteFails(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{
		repos: []string{"app"},
		tags:  map[string][]string{"app": {"v1"}},
		digErr: map[string]error{
			"app\x00v1": errors.New("boom"),
		},
	}
	if _, err := Run(ctx, errWriter{}, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil); err == nil {
		t.Error("digest warning over dead pipe succeeded, want the write failure")
	}
}
