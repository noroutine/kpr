package gc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// removeHusks deletes tagless repo dirs: nothing pullable lives
// there, so removal only orphans blobs the next collect owns. A
// tagged repo stays, a sentinel-prefix repo stays (machinery, never
// inventory), and a husk with a live upload session stays (a push in
// flight may still be tagging). A manifest-less dir (bare) is
// skipped without aborting the walk. Names come back sorted for the
// report. If this fails, gc either deletes live inventory or leaves
// husks no later pass can find.
// No skeleton, no husks: a root without the registry layout
// classifies nothing instead of failing the stat. If this fails,
// fresh roots refuse collection.
func TestRemoveHusksMissingLayoutIsNil(t *testing.T) {
	got, err := removeHusks(t.TempDir())
	if err != nil {
		t.Fatalf("removeHusks over bare root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("removeHusks over bare root = %v, want nil", got)
	}
}

// A tagged repo nested under a husk vetoes the ancestor's
// removal: the parent may be dead layout, but the child is live
// inventory. If this fails, gc wipes a live repo with its dead
// parent.
func TestRemoveHusksSparesNestedKept(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2", "repositories")
	for _, rel := range []string{
		"outer/_manifests/revisions/sha256/a/link",
		"outer/inner/_manifests/tags/v1/current/link",
		"outer/inner/_manifests/revisions/sha256/b/link",
	} {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte("sha256:x"), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	got, err := removeHusks(root)
	if err != nil {
		t.Fatalf("removeHusks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("removeHusks = %v, want empty (nested kept vetoes the husk)", got)
	}
	if _, err := os.Stat(filepath.Join(v2, "outer")); err != nil {
		t.Errorf("nested-kept ancestor removed: %v", err)
	}
}

// A removed husk takes its tagless children with it without
// naming them: sorted parents first, children skipped as gone.
// If this fails, the report double-counts nested husks.
func TestRemoveHusksSkipsRemovedChildren(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2", "repositories")
	for _, rel := range []string{
		"p/_manifests/revisions/sha256/a/link",
		"p/c/_manifests/revisions/sha256/b/link",
	} {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte("sha256:x"), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	got, err := removeHusks(root)
	if err != nil {
		t.Fatalf("removeHusks: %v", err)
	}
	if len(got) != 1 || got[0] != "p" {
		t.Errorf("removeHusks = %v, want [p] (child skipped as gone)", got)
	}
}

// The preview lists exactly what arming removes: findHusks on a
// staged tree must equal the removal report on the same state
// (find mutates nothing, so sequential agreement is the promise).
// If this fails, the preview counts husks arming wouldn't take.
func TestFindHusksMatchesRemoveHusks(t *testing.T) {
	stage := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		v2 := filepath.Join(root, "docker", "registry", "v2", "repositories")
		for _, rel := range []string{
			"gone/_manifests/revisions/sha256/a/link",
			"live/_manifests/tags/v1/current/link",
			"live/_manifests/revisions/sha256/b/link",
			"outer/_manifests/revisions/sha256/c/link",
			"outer/inner/_manifests/revisions/sha256/d/link",
		} {
			p := filepath.Join(v2, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatalf("stage dir: %v", err)
			}
			if err := os.WriteFile(p, []byte("sha256:x"), 0o644); err != nil {
				t.Fatalf("stage file: %v", err)
			}
		}
		return root
	}
	root := stage(t)
	want, err := findHusks(root)
	if err != nil {
		t.Fatalf("findHusks: %v", err)
	}
	got, err := removeHusks(root)
	if err != nil {
		t.Fatalf("removeHusks: %v", err)
	}
	if !slices.Equal(want, got) {
		t.Errorf("findHusks = %v, removeHusks = %v, want agreement", want, got)
	}
	if len(want) != 2 {
		t.Errorf("agreement on %v, want [gone outer] (live spared)", want)
	}
}

// Unreadable layout refuses instead of classifying blind: a
// blinded uploads tree hides live pushes, a blinded tags tree
// hides live tags. Root reads through permissions, so it sits
// this one out. If this fails, blind spots classify as idle.
func TestRemoveHusksUnreadableRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	stage := func(t *testing.T, blind string) string {
		t.Helper()
		root := t.TempDir()
		v2 := filepath.Join(root, "docker", "registry", "v2", "repositories", "app")
		for _, rel := range []string{
			"_manifests/revisions/sha256/a/link",
			"_manifests/tags/v1/current/link",
			"_uploads/uuid-1/data",
		} {
			p := filepath.Join(v2, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatalf("stage dir: %v", err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatalf("stage file: %v", err)
			}
		}
		bp := filepath.Join(v2, blind)
		if err := os.Chmod(bp, 0o000); err != nil {
			t.Fatalf("blind %s: %v", blind, err)
		}
		t.Cleanup(func() { _ = os.Chmod(bp, 0o755) })
		return root
	}
	for _, blind := range []string{"_uploads", filepath.Join("_manifests", "tags")} {
		if _, err := removeHusks(stage(t, blind)); err == nil {
			t.Errorf("removeHusks over blinded %s succeeded, want refusal", blind)
		}
	}
}

// An unwritable repos dir fails the removal loud with what went
// nowhere: half-removed inventory must shout, never guess. If
// this fails, permission errors vanish into a nil return.
func TestRemoveHusksRefusesUnwritableRepo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes through file permissions")
	}
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2", "repositories")
	p := filepath.Join(v2, "husk", "_manifests", "revisions", "sha256", "a", "link")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.WriteFile(p, []byte("sha256:x"), 0o644); err != nil {
		t.Fatalf("stage file: %v", err)
	}
	if err := os.Chmod(v2, 0o555); err != nil {
		t.Fatalf("blind repos: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(v2, 0o755) })
	if _, err := removeHusks(root); err == nil {
		t.Error("remove over unwritable repos succeeded, want refusal")
	}
}

// A blinded parent refuses the find: statting through a
// permission wall is unknown, never empty. If this fails, blind
// spots list as husk-free.
func TestFindHusksRefusesBlindParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root stats through file permissions")
	}
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	if err := os.MkdirAll(v2, 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.Chmod(v2, 0o000); err != nil {
		t.Fatalf("blind parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(v2, 0o755) })
	if _, err := findHusks(root); err == nil {
		t.Error("find over blinded parent succeeded, want refusal")
	}
}

// A blinded repos dir refuses the walk: listing through a
// permission wall is unknown, never done. If this fails, blind
// spots walk as empty.
func TestFindHusksRefusesBlindRepos(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2", "repositories")
	if err := os.MkdirAll(v2, 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.Chmod(v2, 0o000); err != nil {
		t.Fatalf("blind repos: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(v2, 0o755) })
	if _, err := findHusks(root); err == nil {
		t.Error("find over blinded repos succeeded, want refusal")
	}
}

func TestRemoveHusksDeletesOnlyTrueHusks(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2", "repositories")
	files := map[string]string{
		"live/_manifests/tags/v1/current/link":                    "sha256:aaa",
		"live/_manifests/revisions/sha256/aaa/link":               "sha256:aaa",
		"husk/_manifests/revisions/sha256/bbb/link":               "sha256:bbb",
		"husk/_layers/sha256/111/link":                            "sha256:111",
		"nest/husk/_manifests/revisions/sha256/c/link":            "sha256:ccc",
		"noroutine/kpr-shadow/_manifests/revisions/sha256/d/link": "sha256:ddd",
		"busy/_manifests/revisions/sha256/e/link":                 "sha256:eee",
		"busy/_uploads/uuid-1/data":                               "partial",
		"bare/_layers/sha256/111/link":                            "sha256:111",
		"stale/_manifests/revisions/sha256/f/link":                "sha256:fff",
		"stale/_uploads/uuid-9/data":                              "partial",
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
	// A tags dir that walks clean but holds no tag still classifies
	// as tagless: a successful walk is not evidence of tags. If this
	// fails, present-but-empty tags dirs read as kept and never
	// collect.
	if err := os.MkdirAll(filepath.Join(v2, "cleartags", "_manifests", "tags"), 0o755); err != nil {
		t.Fatalf("stage empty tags: %v", err)
	}
	// A stray file named _manifests does not end the descent: nested
	// repos beneath it still classify. If this fails, a misplaced
	// file hides a whole subtree from collection.
	stray := filepath.Join(v2, "stray", "nest-husk", "_manifests", "revisions", "sha256", "h", "link")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatalf("stage nested husk: %v", err)
	}
	if err := os.WriteFile(stray, []byte("sha256:h"), 0o644); err != nil {
		t.Fatalf("stage nested link: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v2, "stray", "_manifests"), []byte("stray"), 0o644); err != nil {
		t.Fatalf("stage stray file: %v", err)
	}
	// Crash residue from long ago never vetoes: only a session
	// touched within the stale age may still be tagging.
	old := time.Now().Add(-48 * time.Hour)
	staleSession := filepath.Join(v2, "stale", "_uploads", "uuid-9")
	if err := os.Chtimes(filepath.Join(staleSession, "data"), old, old); err != nil {
		t.Fatalf("age residue: %v", err)
	}
	if err := os.Chtimes(staleSession, old, old); err != nil {
		t.Fatalf("age residue: %v", err)
	}
	got, err := removeHusks(root)
	if err != nil {
		t.Fatalf("removeHusks: %v", err)
	}
	if strings.Join(got, ",") != "cleartags,husk,nest/husk,stale,stray/nest-husk" {
		t.Fatalf("removed %v, want [cleartags husk nest/husk stale stray/nest-husk]", got)
	}
	for _, kept := range []string{"live", "noroutine/kpr-shadow", "busy"} {
		if _, err := os.Lstat(filepath.Join(v2, filepath.FromSlash(kept))); err != nil {
			t.Errorf("%s unreadable, want kept: %v", kept, err)
		}
	}
	for _, gone := range []string{"cleartags", "husk", "nest/husk", "stale", "stray/nest-husk"} {
		if _, err := os.Lstat(filepath.Join(v2, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s survives, want removed", gone)
		}
	}
}
