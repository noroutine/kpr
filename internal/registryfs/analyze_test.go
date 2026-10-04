package registryfs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/proof"
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
		Blobs: 2, BlobBytes: 18, DanglingTags: 4,
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

// An unreadable upload session fails the walk: the uploader's
// half-state must abort analysis, never silently uncount. If this
// fails, permission trouble mid-layout reads as clean.
func TestAnalyzeUnreadableUploadsFails(t *testing.T) {
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
		Blobs: 1, BlobBytes: 6, DanglingTags: 1, DanglingLayers: 1}
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
	if !reflect.DeepEqual(got, Report{}) {
		t.Errorf("Analyze empty = %+v, want zeros", got)
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
