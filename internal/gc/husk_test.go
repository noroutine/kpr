package gc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RemoveHusks deletes tagless repo dirs: nothing pullable lives
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
	got, err := RemoveHusks(t.TempDir())
	if err != nil {
		t.Fatalf("RemoveHusks over bare root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("RemoveHusks over bare root = %v, want nil", got)
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
	got, err := RemoveHusks(root)
	if err != nil {
		t.Fatalf("RemoveHusks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("RemoveHusks = %v, want empty (nested kept vetoes the husk)", got)
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
	got, err := RemoveHusks(root)
	if err != nil {
		t.Fatalf("RemoveHusks: %v", err)
	}
	if len(got) != 1 || got[0] != "p" {
		t.Errorf("RemoveHusks = %v, want [p] (child skipped as gone)", got)
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
		if _, err := RemoveHusks(stage(t, blind)); err == nil {
			t.Errorf("RemoveHusks over blinded %s succeeded, want refusal", blind)
		}
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
	got, err := RemoveHusks(root)
	if err != nil {
		t.Fatalf("RemoveHusks: %v", err)
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
