package gc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RemoveHusks deletes tagless repo dirs: nothing pullable lives
// there, so removal only orphans blobs the next collect owns. A
// tagged repo stays, a sentinel-prefix repo stays (machinery, never
// inventory), and a husk with a live upload session stays (a push in
// flight may still be tagging). Names come back sorted for the
// report. If this fails, gc either deletes live inventory or leaves
// husks no later pass can find.
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
	got, err := RemoveHusks(root)
	if err != nil {
		t.Fatalf("RemoveHusks: %v", err)
	}
	if strings.Join(got, ",") != "husk,nest/husk" {
		t.Fatalf("removed %v, want [husk nest/husk]", got)
	}
	for _, kept := range []string{"live", "noroutine/kpr-shadow", "busy"} {
		if _, err := os.Lstat(filepath.Join(v2, filepath.FromSlash(kept))); err != nil {
			t.Errorf("%s unreadable, want kept: %v", kept, err)
		}
	}
	for _, gone := range []string{"husk", "nest/husk"} {
		if _, err := os.Lstat(filepath.Join(v2, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s survives, want removed", gone)
		}
	}
}
