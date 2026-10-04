package registryfs

import (
	"os"
	"path/filepath"
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
	if got != want {
		t.Errorf("Analyze = %+v, want %+v", got, want)
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
	if got != want {
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
	if last != want {
		t.Errorf("last progress = %+v, want %+v", last, want)
	}
	if got != want {
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
	if got != (Report{}) {
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
