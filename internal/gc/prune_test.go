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

// An unreadable subdir fails the run loud: silently skipping what
// cannot be listed would report a clean store over unknown state.
// Root reads through permissions, so it sits this one out.
func TestPruneEmptyDirsRefusesUnreadableSubdir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through directory permissions")
	}
	root := t.TempDir()
	dark := filepath.Join(root, "dark")
	if err := os.MkdirAll(dark, 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.Chmod(dark, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dark, 0o755) })
	if _, err := PruneEmptyDirs(root); err == nil {
		t.Error("unreadable subdir pruned clean, want refusal")
	}
}

// Structural skeleton dirs survive empty: the stock collector stats
// _layers (and reads tags) per repo during mark, so a pruned _layers
// aborts the whole next collect (observed live: Path not found on
// infra-dev/rabbitmq-cluster-operator, zero deletions). Only the
// containers are spared — emptied contents (a dead tag dir, a spent
// revision digest, a blob shard) still prune. If this fails, every gc
// after the first is dead on any volume prune ever touched.
func TestPruneEmptyDirsSparesRegistrySkeleton(t *testing.T) {
	root := t.TempDir()
	skeleton := []string{
		filepath.Join("repositories", "r1", "_layers"),
		filepath.Join("repositories", "r1", "_manifests", "tags"),
		filepath.Join("repositories", "r1", "_manifests", "revisions"),
		filepath.Join("repositories", "r1", "_uploads"),
		filepath.Join("blobs", "sha256"),
	}
	for _, dir := range skeleton {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("stage skeleton %s: %v", dir, err)
		}
	}
	// Emptied contents prune normally: a dead tag dir, a spent
	// revision digest, an emptied blob shard, a stray empty dir.
	contents := []string{
		filepath.Join("repositories", "r1", "_manifests", "tags", "v9"),
		filepath.Join("repositories", "r1", "_manifests", "revisions", "sha256", "abc"),
		filepath.Join("blobs", "sha256", "ab"),
		"stray",
	}
	for _, dir := range contents {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("stage content %s: %v", dir, err)
		}
	}
	if _, err := PruneEmptyDirs(root); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, dir := range skeleton {
		if _, err := os.Lstat(filepath.Join(root, dir)); err != nil {
			t.Errorf("skeleton %s unreadable, want kept: %v", dir, err)
		}
	}
	for _, dir := range contents {
		if _, err := os.Lstat(filepath.Join(root, dir)); !os.IsNotExist(err) {
			t.Errorf("emptied %s survives, want pruned", dir)
		}
	}
}

// An unremovable dir fails the run loud for the same reason: the
// remove is the emptiness check, so anything but occupancy or
// absence resisting it wants the operator. Root unlinks through
// permissions, so it sits this one out too.
func TestPruneEmptyDirsRefusesUnwritableSubdir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks through directory permissions")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "child"), 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := PruneEmptyDirs(root); err == nil {
		t.Error("unwritable subdir pruned clean, want refusal")
	}
}
