package gc

import (
	"os"
	"path/filepath"
	"testing"
)

// The collector deletes blobs and links but leaves their parents:
// an empty a/b/c, a dir holding a file, a lone file, and a
// symlink must come back as gone / kept / kept / kept, with root
// itself surviving. If this fails, gc either deletes live state
// or stops cleaning the skeleton.
func TestPruneEmptyDirsKeepsLiveState(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755); err != nil {
		t.Fatalf("stage skeleton: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "x", "y"), 0o755); err != nil {
		t.Fatalf("stage skeleton: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "x", "y", "blob"), []byte("live"), 0o644); err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose"), []byte("live"), 0o644); err != nil {
		t.Fatalf("stage file: %v", err)
	}
	if err := os.Symlink("x", filepath.Join(root, "link")); err != nil {
		t.Fatalf("stage symlink: %v", err)
	}

	removed, err := PruneEmptyDirs(root)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 3 {
		t.Errorf("pruned %d dirs, want 3 (a/b/c)", removed)
	}
	for _, gone := range []string{"a", filepath.Join("a", "b"), filepath.Join("a", "b", "c")} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survives, want pruned", gone)
		}
	}
	for _, kept := range []string{"", "x", filepath.Join("x", "y"), filepath.Join("x", "y", "blob"), "loose", "link"} {
		if _, err := os.Lstat(filepath.Join(root, kept)); err != nil {
			t.Errorf("%s unreadable, want kept: %v", kept, err)
		}
	}
}

// An absent root refuses loud, never silent: pruning nothing
// while reporting success would hide a wrong store path.
func TestPruneEmptyDirsRefusesAbsentRoot(t *testing.T) {
	if _, err := PruneEmptyDirs(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("absent root pruned clean, want refusal")
	}
}
