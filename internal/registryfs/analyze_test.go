package registryfs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// proveRoot wraps root in a filestore proof: every Analyze test
// walks through the token, never a bare path.
func proveRoot(t *testing.T, root string) proof.FilesystemStore {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	fstore, err := proof.ProveFilesystemStore(cfg)
	if err != nil {
		t.Fatalf("prove staged store: %v", err)
	}
	return fstore
}

// stageLayout builds a v2 store with nesting and hostile names: a
// nested repo, a repo literally named "tags" holding a tag
// literally named "revisions", an index link in real shape, junk
// files under an upload session, and a sha512 revision. Expected:
// Repos 3, Tags 4 (index link excluded), Revisions 3 (both
// algorithms), LayerLinks 2, Uploads 1 (junk uncounted), Blobs 2
// (6 + 12 bytes). Four tag links dangle by construction (v2 and
// the hostile names point at no revision); both layer links hold.
func stageLayout(t *testing.T) (proof.FilesystemStore, Report) {
	t.Helper()
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/app/_manifests/tags/v1/current/link":                 "sha256:aaa",
		"repositories/app/_manifests/tags/v2/current/link":                 "sha256:bbb",
		"repositories/app/_manifests/revisions/sha256/aaa/link":            "sha256:aaa",
		"repositories/app/_manifests/revisions/sha512/ddd/link":            "sha512:ddd",
		"repositories/app/_layers/sha256/111/link":                         "sha256:111",
		"repositories/app/_layers/sha256/222/link":                         "sha256:222",
		"repositories/app/_uploads/uuid-1/startedAt":                       "x",
		"repositories/app/_uploads/uuid-1/data":                            "partial",
		"repositories/app/_uploads/uuid-1/stray-link":                      "junk",
		"repositories/nest/deep/_manifests/tags/10s/current/link":          "sha256:ccc",
		"repositories/nest/deep/_manifests/tags/10s/index/sha256/eee/link": "sha256:eee",
		"repositories/nest/deep/_manifests/revisions/sha256/ccc/link":      "sha256:ccc",
		"repositories/tags/_manifests/tags/revisions/current/link":         "sha256:fff",
		"repositories/app/_manifests/tags/_uploads/current/link":           "sha256:111",
		"repositories/app/_manifests/tags/_manifests/current/link":         "sha256:222",
		"blobs/sha256/11/111/data":                                         "123456",
		"blobs/sha256/22/222/data":                                         "123456789012",
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	return proveRoot(t, root), Report{
		Repos: 3, Tags: 6, Revisions: 3, LayerLinks: 2, Uploads: 1,
		Blobs: 2, BlobBytes: 18, DanglingTags: 4, Prime: sentinel.PrimeMissing,
	}
}

// The walk resolves pointer classes the counting pass cannot:
// tag links whose target revision link is absent, layer links
// whose blob data is absent. End-exact: progress middles may show
// zeros, the returned report never does. If this fails, dead
// pointers hide inside healthy magnitudes.
// ListRepos is the ghosts listing's fs witness: every repo holding
// a _manifests dir, tagged or not — only _manifests presence proves
// the repo exists on fs, a bare dir does not. Nil proof refuses; an
// absent layout yields nil (unproven: a fresh volume and an
// unmounted one look identical, and absence of view is never
// evidence). If this fails, the ghosts view reads the wrong fs and
// either hides deletions or names live repos.
func TestListReposNamesManifestDirs(t *testing.T) {
	fstore, _ := stageLayout(t)
	bare := filepath.Join(fstore.Root(), "docker", "registry", "v2", "repositories", "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("stage bare dir: %v", err)
	}
	got, err := ListRepos(fstore)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	want := map[string]bool{"app": true, "nest/deep": true, "tags": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListRepos = %v, want %v (bare dir excluded)", got, want)
	}
	if _, err := ListRepos(nil); err == nil {
		t.Error("ListRepos(nil) succeeded, want refusal")
	}
	empty, err := ListRepos(proveRoot(t, t.TempDir()))
	if err != nil {
		t.Fatalf("ListRepos empty layout: %v", err)
	}
	if empty != nil {
		t.Errorf("ListRepos empty layout = %v, want nil (unproven, not proven-empty)", empty)
	}
}

// A path too short to hold a repo never names a sentinel tag: no
// _manifests anchor, no verdict. If this fails, shallow paths
// classify as machinery.
func TestIsSentinelTagShortPath(t *testing.T) {
	for _, parts := range [][]string{{}, {"app"}, {"app", "_manifests"}} {
		if isSentinelTag(parts) {
			t.Errorf("isSentinelTag(%v) = true, want false (no repo component)", parts)
		}
	}
}

// Files inside the repositories dir are not repos: WalkDir meets
// them and moves on. If this fails, stray files list as repos.
func TestListReposSkipsFiles(t *testing.T) {
	root := t.TempDir()
	repos := filepath.Join(root, "docker", "registry", "v2", "repositories")
	if err := os.MkdirAll(repos, 0o755); err != nil {
		t.Fatalf("stage repos: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repos, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatalf("stage stray file: %v", err)
	}
	got, err := ListRepos(proveRoot(t, root))
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListRepos = %v, want empty (files are not repos)", got)
	}
}

// Unreadable layout fails both light walks instead of reading
// partial: a blinded parent names itself in the error. If this
// fails, permission loss lists half a registry as whole.
func TestLightWalksUnreadableFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	root := t.TempDir()
	repos := filepath.Join(root, "docker", "registry", "v2", "repositories", "app")
	if err := os.MkdirAll(filepath.Join(repos, "_manifests"), 0o755); err != nil {
		t.Fatalf("stage repo: %v", err)
	}
	if err := os.Chmod(repos, 0o000); err != nil {
		t.Fatalf("blind repo: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(repos, 0o755) })
	fstore := proveRoot(t, root)
	if _, err := ListRepos(fstore); err == nil {
		t.Error("ListRepos over blinded repo succeeded, want failure")
	}
	if _, err := ListHusks(fstore); err == nil {
		t.Error("ListHusks over blinded repo succeeded, want failure")
	}
	// A blinded skeleton names itself too: the repositories and
	// v2 stats refuse instead of reading absence. Restore runs
	// parent-first: a blinded v2 would deny the chmod below it.
	v2 := filepath.Join(root, "docker", "registry", "v2")
	v2repos := filepath.Join(v2, "repositories")
	unblind := func(t *testing.T) {
		t.Helper()
		for _, dir := range []string{v2, v2repos, repos} {
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatalf("unblind %s: %v", dir, err)
			}
		}
	}
	for _, tc := range []struct{ name, dir string }{
		{"repositories", v2repos},
		{"v2", v2},
	} {
		unblind(t)
		if err := os.Chmod(tc.dir, 0o000); err != nil {
			t.Fatalf("blind %s: %v", tc.name, err)
		}
		t.Cleanup(func() { _ = os.Chmod(tc.dir, 0o755) })
		if _, err := ListRepos(fstore); err == nil {
			t.Errorf("ListRepos over blinded %s succeeded, want failure", tc.name)
		}
	}
	unblind(t)
	// A blinded tags dir fails the husk verdict after a readable
	// _manifests: the candidate stands, its tags don't.
	tags := filepath.Join(repos, "_manifests", "tags")
	if err := os.MkdirAll(tags, 0o755); err != nil {
		t.Fatalf("stage tags: %v", err)
	}
	if err := os.Chmod(tags, 0o000); err != nil {
		t.Fatalf("blind tags: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(tags, 0o755) })
	if _, err := ListHusks(fstore); err == nil {
		t.Error("ListHusks over blinded tags succeeded, want failure")
	}
}

// _layers and _uploads dirs skip wholesale: session and layer
// areas never read as husk candidates. If this fails, machinery
// dirs list as tagless repos.
func TestListHusksSkipsLayerDirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"_layers", "_uploads"} {
		p := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", dir, "_manifests")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("stage %s: %v", dir, err)
		}
	}
	got, err := ListHusks(proveRoot(t, root))
	if err != nil {
		t.Fatalf("ListHusks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListHusks = %v, want empty (layer areas skipped)", got)
	}
}

// revisions is a legal repo path component (only _-prefixed names
// are reserved for layout machinery): team/revisions must classify
// on every walk — the full analyze, the husk listing, and ListRepos
// alike. If this fails, the light walks silently drop real repos
// the full walk counts.
func TestRevisionsComponentClassifies(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/team/revisions/_manifests/tags/v1/current/link":       "sha256:aaa",
		"repositories/team/revisions/_manifests/revisions/sha256/aaa/link":  "sha256:aaa",
		"repositories/stale/revisions/_manifests/revisions/sha256/bbb/link": "sha256:bbb",
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	fstore := proveRoot(t, root)
	if got, err := ListRepos(fstore); err != nil {
		t.Fatalf("ListRepos: %v", err)
	} else if !got["team/revisions"] || !got["stale/revisions"] {
		t.Errorf("ListRepos = %v, want team/revisions and stale/revisions", got)
	}
	husks, err := ListHusks(fstore)
	if err != nil {
		t.Fatalf("ListHusks: %v", err)
	}
	if len(husks) != 1 || husks[0] != "stale/revisions" {
		t.Errorf("ListHusks = %v, want [stale/revisions]", husks)
	}
	rep, err := Analyze(fstore, nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rep.Repos != 2 {
		t.Errorf("Analyze Repos = %d, want 2", rep.Repos)
	}
}

// Refusals name the path they tripped on: a missing root, a file
// where the root should be, and a layout without repositories
// (zeros, not an error — a fresh volume holds no repos). If this
// fails, operators get a bare error (or a panic) where the remedy
// is the path itself.
func TestWalksRefuseBlindPaths(t *testing.T) {
	missing := proveRoot(t, filepath.Join(t.TempDir(), "nope"))
	for name, call := range map[string]func() error{
		"analyze": func() error { _, err := Analyze(missing, nil); return err },
		"husks":   func() error { _, err := ListHusks(missing); return err },
		"repos":   func() error { _, err := ListRepos(missing); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s over missing root succeeded, want the path named", name)
		}
	}
	fileRoot := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o644); err != nil {
		t.Fatalf("stage file root: %v", err)
	}
	fstore := proveRoot(t, fileRoot)
	if _, err := Analyze(fstore, nil); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("analyze over file root = %v, want not-a-directory", err)
	}
	if _, err := ListHusks(fstore); err == nil {
		t.Error("husks over file root succeeded, want refusal")
	}
	if _, err := ListRepos(fstore); err == nil {
		t.Error("repos over file root succeeded, want refusal")
	}
	bare := proveRoot(t, t.TempDir())
	if rep, err := Analyze(bare, nil); err != nil || rep.Repos != 0 {
		t.Errorf("analyze over layout-less root = %+v, %v, want zeros", rep, err)
	}
	if husks, err := ListHusks(bare); err != nil || husks != nil {
		t.Errorf("husks over layout-less root = %v, %v, want nil, nil", husks, err)
	}
	if repos, err := ListRepos(bare); err != nil || repos != nil {
		t.Errorf("repos over layout-less root = %v, %v, want nil, nil", repos, err)
	}
	v2only := t.TempDir()
	if err := os.MkdirAll(filepath.Join(v2only, "docker", "registry", "v2"), 0o755); err != nil {
		t.Fatalf("stage v2-only: %v", err)
	}
	vs := proveRoot(t, v2only)
	if rep, err := Analyze(vs, nil); err != nil || rep.Repos != 0 {
		t.Errorf("analyze without repositories = %+v, %v, want zeros", rep, err)
	}
	if husks, err := ListHusks(vs); err != nil || husks != nil {
		t.Errorf("husks without repositories = %v, %v, want nil, nil", husks, err)
	}
	if repos, err := ListRepos(vs); err != nil || repos != nil {
		t.Errorf("repos without repositories = %v, %v, want nil, nil", repos, err)
	}
}

// An unreadable link fails the walk loudly: tag links and layer
// links both resolve through linkTarget, and a target that cannot
// be read must abort, never skip. One root per shape — the walk
// stops at the first error, so a shared fixture would pin only
// one site. If this fails, corrupt layouts analyze as healthy.
func TestAnalyzeUnreadableLinkFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	stage := func(t *testing.T, link string) proof.FilesystemStore {
		t.Helper()
		root := t.TempDir()
		p := filepath.Join(root, "docker", "registry", "v2", filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage link dir: %v", err)
		}
		if err := os.WriteFile(p, []byte("sha256:aaa"), 0o000); err != nil {
			t.Fatalf("stage link: %v", err)
		}
		return proveRoot(t, root)
	}
	for name, link := range map[string]string{
		"tag":   "repositories/app/_manifests/tags/v1/current/link",
		"layer": "repositories/app/_layers/sha256/111/link",
	} {
		if _, err := Analyze(stage(t, link), nil); err == nil {
			t.Errorf("analyze with unreadable %s link succeeded, want failure", name)
		}
	}
}

// A top-level repositories/_uploads is dead layout, not a repo's
// sessions: the walk counts per-repo areas only, deeper than two.
// If this fails, stray dirs inflate the upload count.
func TestAnalyzeSkipsTopLevelUploads(t *testing.T) {
	root := t.TempDir()
	junk := filepath.Join(root, "docker", "registry", "v2", "repositories", "_uploads", "sess-1")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatalf("stage junk uploads: %v", err)
	}
	got, err := Analyze(proveRoot(t, root), nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got.Uploads != 0 {
		t.Errorf("Analyze.Uploads = %d, want 0 (dead layout uncounted)", got.Uploads)
	}
}

// Progress fires mid-walk and stays sparse: a long walk reports
// before the final line, but per-visit snapshots would drown the
// pipe. The cadence is a tuning (not pinned exact), the bounds
// are the contract — at least one mid report on a long walk,
// never a flood. If this fails, the live block either never
// updates or spams one line per file.
func TestAnalyzeProgressBounds(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/aaa/_manifests/revisions/sha256/bbb/link": "sha256:bbb",
	}
	for i := 0; i < 1050; i++ {
		files[fmt.Sprintf("repositories/zzz/_layers/sha256/d%04d/link", i)] = "sha256:111"
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	var mids int
	var last Report
	got, err := Analyze(proveRoot(t, root), func(rep Report) {
		mids++
		last = rep
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if mids < 2 {
		t.Errorf("progress reports = %d, want mid-walk plus final", mids)
	}
	if mids > 100 {
		t.Errorf("progress reports = %d, want sparse, not per-visit", mids)
	}
	if last.Husks != got.Husks || len(last.HuskRepos) != len(got.HuskRepos) {
		t.Errorf("last mid %+v, want the final verdict %+v", last, got)
	}
}

// A nested repo sorting before _manifests still classifies and
// clears its parent: the open stack holds the current ancestry,
// so the parent is top-of-stack at its tag's visit even though
// 0sub opened earlier in lexical order. If this fails,
// early-sorting nested repos orphan their parent's husk verdict.
func TestAnalyzeNestedEarlyRepoClearsParent(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/r/_manifests/tags/v1/current/link":           "sha256:aaa",
		"repositories/r/0sub/_manifests/revisions/sha256/bbb/link": "sha256:bbb",
		"repositories/r/0sub/_manifests/tags/nightly/current/link": "sha256:bbb",
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	got, err := Analyze(proveRoot(t, root), nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got.Repos != 2 || got.Tags != 2 {
		t.Errorf("Analyze = %+v, want 2 repos 2 tags", got)
	}
	for _, h := range got.HuskRepos {
		if h == "r" {
			t.Errorf("HuskRepos = %v, want r cleared by its tag", got.HuskRepos)
		}
	}
}

// An unreadable upload session fails the walk: the uploader's
// half-state must abort analysis, never silently uncount. If this
// fails, permission trouble mid-layout reads as clean.
func TestAnalyzeUnreadableUploadsFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through directory permissions")
	}
	root := t.TempDir()
	up := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_uploads", "uuid-1")
	if err := os.MkdirAll(up, 0o755); err != nil {
		t.Fatalf("stage uploads: %v", err)
	}
	if err := os.Chmod(filepath.Dir(up), 0o000); err != nil {
		t.Fatalf("chmod uploads: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(up), 0o755) })
	if _, err := Analyze(proveRoot(t, root), nil); err == nil {
		t.Error("analyze over unreadable uploads succeeded, want failure")
	}
}

func TestAnalyzeReportsDanglingLinks(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/app/_manifests/tags/ok/current/link":      "sha256:aaa",
		"repositories/app/_manifests/tags/dead/current/link":    "sha256:bbb",
		"repositories/app/_manifests/revisions/sha256/aaa/link": "sha256:aaa",
		"repositories/app/_layers/sha256/111/link":              "sha256:111",
		"repositories/app/_layers/sha256/222/link":              "sha256:222",
		"blobs/sha256/11/111/data":                              "123456",
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	got, err := Analyze(proveRoot(t, root), nil)
	if err != nil {
		t.Fatalf("Analyze = %v, want counts", err)
	}
	want := Report{Repos: 1, Tags: 2, Revisions: 1, LayerLinks: 2,
		Blobs: 1, BlobBytes: 6, DanglingTags: 1, DanglingLayers: 1, Prime: sentinel.PrimeMissing}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Analyze = %+v, want %+v", got, want)
	}
}

// A repo holding _manifests but no tag links is a husk: swept
// bare, collected, or never tagged. The walk counts it and names it
// (sorted); a sentinel-prefix repo is machinery, never inventory,
// even tagless. If this fails, husk repos hide inside the repo
// magnitude and no remover can find them.
func TestAnalyzeReportsHusks(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/app/_manifests/tags/v1/current/link":                     "sha256:aaa",
		"repositories/app/_manifests/revisions/sha256/aaa/link":                "sha256:aaa",
		"repositories/bare/_manifests/revisions/sha256/bbb/link":               "sha256:bbb",
		"repositories/bare/_layers/sha256/111/link":                            "sha256:111",
		"repositories/nest/husk/_manifests/revisions/sha256/ccc/link":          "sha256:ccc",
		"repositories/noroutine/kpr-shadow/_manifests/revisions/sha256/d/link": "sha256:ddd",
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	got, err := Analyze(proveRoot(t, root), nil)
	if err != nil {
		t.Fatalf("Analyze = %v, want counts", err)
	}
	if got.Husks != 2 {
		t.Errorf("Husks = %d, want 2 (bare, nest/husk)", got.Husks)
	}
	want := []string{"bare", "nest/husk"}
	if strings.Join(got.HuskRepos, ",") != strings.Join(want, ",") {
		t.Errorf("HuskRepos = %v, want %v", got.HuskRepos, want)
	}
}

// Husk verdicts go live: a repo finalizes the moment the walk
// steps out of its subtree, so a mid-walk progress already counts
// confirmed husks instead of dumping 150 at the end. If this
// fails, husks arrive only with the final report and the live
// block misleads for the whole walk.
func TestAnalyzeReportsHusksLive(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/aaa/_manifests/revisions/sha256/bbb/link": "sha256:bbb",
	}
	for i := 0; i < 1100; i++ {
		files[fmt.Sprintf("repositories/zzz/_layers/sha256/d%04d/link", i)] = "sha256:111"
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	var mids []Report
	got, err := Analyze(proveRoot(t, root), func(rep Report) {
		mids = append(mids, rep)
	})
	if err != nil {
		t.Fatalf("Analyze = %v, want counts", err)
	}
	if got.Husks != 1 {
		t.Fatalf("Husks = %d, want 1", got.Husks)
	}
	for _, mid := range mids[:len(mids)-1] {
		if mid.Husks == 1 {
			return
		}
	}
	t.Errorf("no mid-walk progress showed the husk (%d progress calls)", len(mids))
}

// The husk listing agrees with the full walk on the same layout:
// one entry per tagless repo, sorted, sentinel machinery excluded
// — the fast path `registry ls husks` takes must never disagree
// with analyze. If this fails, the listing and the report name
// different husks.
func TestListHusksMatchesAnalyze(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/app/_manifests/tags/v1/current/link":                     "sha256:aaa",
		"repositories/app/_manifests/revisions/sha256/aaa/link":                "sha256:aaa",
		"repositories/bare/_manifests/revisions/sha256/bbb/link":               "sha256:bbb",
		"repositories/nest/husk/_manifests/revisions/sha256/c/link":            "sha256:ccc",
		"repositories/noroutine/kpr-shadow/_manifests/revisions/sha256/d/link": "sha256:ddd",
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	store := proveRoot(t, root)
	got, err := ListHusks(store)
	if err != nil {
		t.Fatalf("ListHusks: %v", err)
	}
	rep, err := Analyze(store, nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if strings.Join(got, ",") != "bare,nest/husk" {
		t.Errorf("ListHusks = %v, want [bare nest/husk]", got)
	}
	if strings.Join(got, ",") != strings.Join(rep.HuskRepos, ",") {
		t.Errorf("ListHusks = %v, Analyze names %v", got, rep.HuskRepos)
	}
}

// The walker counts every file kind exactly once on nesting and
// hostile names. If this fails, analyze reports fiction.
func TestAnalyzeCountsLayout(t *testing.T) {
	fstore, want := stageLayout(t)
	got, err := Analyze(fstore, nil)
	if err != nil {
		t.Fatalf("Analyze = %v, want counts", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Analyze = %+v, want %+v", got, want)
	}
}

// Progress fires every 1024 visits and once at the end with the
// final report: the caller renders live counters from it. If
// this fails, counters never move.
func TestAnalyzeProgressReports(t *testing.T) {
	fstore, want := stageLayout(t)
	var last Report
	n := 0
	got, err := Analyze(fstore, func(rep Report) {
		last, n = rep, n+1
	})
	if err != nil {
		t.Fatalf("Analyze = %v, want counts", err)
	}
	if n == 0 {
		t.Fatal("no progress reported")
	}
	if !reflect.DeepEqual(last, want) {
		t.Errorf("last progress = %+v, want %+v", last, want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Analyze = %+v, want %+v", got, want)
	}
}

// A root with no v2 tree yet is a fresh store: zeros, not a
// refusal. If this fails, empty registries refuse.
func TestAnalyzeEmptyRootZeroes(t *testing.T) {
	got, err := Analyze(proveRoot(t, t.TempDir()), nil)
	if err != nil {
		t.Fatalf("Analyze empty = %v, want zeros", err)
	}
	if !reflect.DeepEqual(got, Report{Prime: sentinel.PrimeMissing}) {
		t.Errorf("Analyze empty = %+v, want zeros with missing prime", got)
	}
}

// A root with no repositories dir reads as no repos, while blobs
// still count: pushes can land data before any repo exists. If
// this fails, dataless layouts mislead.
func TestAnalyzeNoRepositoriesCountsBlobs(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", "11", "111", "data")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.WriteFile(p, []byte("123456"), 0o644); err != nil {
		t.Fatalf("stage file: %v", err)
	}
	got, err := Analyze(proveRoot(t, root), nil)
	if err != nil {
		t.Fatalf("Analyze = %v, want counts", err)
	}
	if got.Repos != 0 || got.Blobs != 1 || got.BlobBytes != 6 {
		t.Errorf("Analyze = %+v, want 0 repos, 1 blob of 6 bytes", got)
	}
}

// A root with no blobs dir reads as no blobs, while repos still
// count: the shards walk independently, neither gates the other.
// Mirrors NoRepositories on the other side of the split. If this
// fails, one shard's absence zeroes the whole report.
func TestAnalyzeNoBlobsCountsRepos(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_manifests", "tags", "v1", "current", "link")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.WriteFile(p, []byte("sha256:aaa"), 0o644); err != nil {
		t.Fatalf("stage file: %v", err)
	}
	got, err := Analyze(proveRoot(t, root), nil)
	if err != nil {
		t.Fatalf("Analyze = %v, want counts", err)
	}
	if got.Repos != 1 || got.Tags != 1 || got.Blobs != 0 || got.BlobBytes != 0 {
		t.Errorf("Analyze = %+v, want 1 repo, 1 tag, 0 blobs", got)
	}
}

// Tags under the sentinel prefix count apart: machinery tags are
// inventory, and the catalog side splits them the same way — the
// comparison only holds when both sides agree on what is what.
// If this fails, sentinel tags inflate the adoptable count.
func TestAnalyzeCountsSentinelTags(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/app/_manifests/tags/v1/current/link":                     "sha256:aaa",
		"repositories/noroutine/kpr-sentinel/_manifests/tags/gen/current/link": "sha256:bbb",
	}
	for rel, body := range files {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	got, err := Analyze(proveRoot(t, root), nil)
	if err != nil {
		t.Fatalf("Analyze = %v, want counts", err)
	}
	if got.Tags != 2 || got.Sentinels != 1 {
		t.Errorf("Analyze = %+v, want 2 tags with 1 sentinel", got)
	}
}

// The prime is a pointer, not inventory: a resolving latest
// annotates present and stays out of both counts; a missing
// link annotates missing; a dangling one, corrupt (still a
// dangling link). If this fails, the pointer inflates
// inventory again.
func TestAnalyzeAnnotatesPrime(t *testing.T) {
	for _, tc := range []struct {
		name      string
		latest    *string
		tags      int
		sentinels int
		prime     sentinel.Prime
		dangling  int
	}{
		{"present", strptr("sha256:bbb"), 1, 1, sentinel.PrimePresent, 0},
		{"missing", nil, 1, 1, sentinel.PrimeMissing, 0},
		{"corrupt", strptr("sha256:zzz"), 1, 1, sentinel.PrimeCorrupt, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			v2 := filepath.Join(root, "docker", "registry", "v2")
			files := map[string]string{
				"repositories/noroutine/kpr-sentinel/_manifests/tags/gen/current/link":     "sha256:bbb",
				"repositories/noroutine/kpr-sentinel/_manifests/revisions/sha256/bbb/link": "sha256:ccc",
			}
			if tc.latest != nil {
				files["repositories/noroutine/kpr-sentinel/_manifests/tags/latest/current/link"] = *tc.latest
			}
			for rel, body := range files {
				p := filepath.Join(v2, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatalf("stage dir: %v", err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatalf("stage file: %v", err)
				}
			}
			got, err := Analyze(proveRoot(t, root), nil)
			if err != nil {
				t.Fatalf("Analyze = %v, want counts", err)
			}
			if got.Tags != tc.tags || got.Sentinels != tc.sentinels || got.Prime != tc.prime || got.DanglingTags != tc.dangling {
				t.Errorf("Analyze = %+v, want %d tags, %d sentinels, prime %q, %d dangling",
					got, tc.tags, tc.sentinels, tc.prime, tc.dangling)
			}
		})
	}
}

func strptr(s string) *string { return &s }

// A nil token refuses before touching the disk: no proof, no
// walk. If this fails, unproven paths analyze.
func TestAnalyzeNilProofRefuses(t *testing.T) {
	if _, err := Analyze(nil, nil); err == nil {
		t.Error("Analyze(nil) succeeded, want refusal")
	}
}

// A missing root fails loudly with the path named: guessing at
// magnitude is refusing. If this fails, missing stores read as
// empty or nameless.
func TestAnalyzeMissingRootRefuses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if _, err := Analyze(proveRoot(t, root), nil); err == nil {
		t.Error("Analyze on absent root succeeded, want refusal")
	} else if !strings.Contains(err.Error(), root) {
		t.Errorf("refusal = %q, want it to name %q", err.Error(), root)
	}
}

// A v2 path that is a file, not a dir, refuses: walking a file
// would silently report zeros. If this fails, files read as
// empty stores.
func TestAnalyzeFileV2Refuses(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	if err := os.MkdirAll(filepath.Dir(v2), 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.WriteFile(v2, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("stage file: %v", err)
	}
	if _, err := Analyze(proveRoot(t, root), nil); err == nil {
		t.Error("Analyze on file v2 succeeded, want refusal")
	}
}

// An unreadable directory aborts with the path named: magnitude
// is exact or refused, never partial. Skipped for root, which
// reads through permissions.
func TestAnalyzeUnreadableDirAborts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through directory permissions")
	}
	fstore, _ := stageLayout(t)
	repodir := filepath.Join(fstore.Root(), "docker", "registry", "v2", "repositories", "app", "_manifests")
	if err := os.Chmod(repodir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(repodir, 0o755) })
	if _, err := Analyze(fstore, nil); err == nil {
		t.Error("Analyze over unreadable dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "_manifests") {
		t.Errorf("refusal = %q, want it to name the path", err.Error())
	}
}

// ListRepos classifies on presence, not readability: a blinded
// _manifests dir still stats as a dir through the readable
// parent, so the repo lists. If this fails, listing second-
// guesses readability the walk cannot see.
func TestListReposListsBlindManifests(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	root := t.TempDir()
	manifests := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_manifests")
	if err := os.MkdirAll(manifests, 0o755); err != nil {
		t.Fatalf("stage repo: %v", err)
	}
	if err := os.Chmod(manifests, 0o000); err != nil {
		t.Fatalf("blind manifests: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifests, 0o755) })
	got, err := ListRepos(proveRoot(t, root))
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !got["app"] {
		t.Errorf("ListRepos = %v, want app (presence, not readability)", got)
	}
}

// A blinded _manifests dir fails the husk verdict at the tags
// descent: the candidate stands, its tags don't, and that is an
// error, never a silent clean. If this fails, permission loss on
// machinery reads as tagless.
func TestListHusksBlindManifestsFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	root := t.TempDir()
	manifests := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_manifests")
	if err := os.MkdirAll(manifests, 0o755); err != nil {
		t.Fatalf("stage repo: %v", err)
	}
	if err := os.Chmod(manifests, 0o000); err != nil {
		t.Fatalf("blind manifests: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifests, 0o755) })
	if _, err := ListHusks(proveRoot(t, root)); err == nil {
		t.Error("ListHusks over blinded _manifests succeeded, want failure")
	}
}

// A blinded _manifests dir fails the full walk too: analyze names
// the outage instead of counting the repo short. If this fails, a
// permission loss reads as a smaller registry.
func TestAnalyzeBlindManifestsFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	root := t.TempDir()
	manifests := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_manifests")
	if err := os.MkdirAll(manifests, 0o755); err != nil {
		t.Fatalf("stage repo: %v", err)
	}
	if err := os.Chmod(manifests, 0o000); err != nil {
		t.Fatalf("blind manifests: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifests, 0o755) })
	if _, err := Analyze(proveRoot(t, root), nil); err == nil {
		t.Error("Analyze over blinded _manifests succeeded, want failure")
	}
}
