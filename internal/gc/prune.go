package gc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// PruneEmptyDirs removes the collector's leftover directory
// skeleton under root, deepest first: the stock collector deletes
// blobs and links but never their parent dirs, so every gc leaves
// an empty tree behind. Only genuinely empty directories go —
// os.Remove is the emptiness check and the delete in one atomic
// step, so a dir that gains a file mid-run survives. Files,
// symlinks, non-empty dirs, structural skeleton containers (which
// the stock walker stats whether or not they hold anything), and
// root itself are never touched: prune stays invisible to the next
// collect.
func PruneEmptyDirs(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("skeleton unreadable at %s: %w", root, err)
	}
	var removed int
	for _, e := range entries {
		n, err := pruneDir(filepath.Join(root, e.Name()))
		if err != nil {
			return removed, err
		}
		removed += n
	}
	return removed, nil
}

// structural reports whether name is a registry skeleton container
// the stock collector stats whether or not it holds anything: the
// top-level repositories and blobs trees, per-repo _layers (a
// missing one aborts the whole mark phase), the tag and revision
// listings, the upload sessions, and the digest algorithm shards
// beneath them. Contents still prune normally — a dead tag dir, a
// spent revision digest, an emptied blob shard — only the
// containers stay, so the next collect walks the tree prune left
// behind without noticing it.
func structural(name string) bool {
	switch name {
	case "repositories", "blobs",
		"_layers", "_manifests", "revisions", "tags", "_uploads",
		"sha256", "sha512", "sha384":
		return true
	}
	return false
}

// pruneDir empties dir bottom-up and removes it when nothing
// remains, reporting how many dirs went in its subtree. A non-dir
// (file, symlink) is left alone; a structural skeleton container is
// recursed but never removed; a dir that resists for any reason but
// occupancy or absence fails the run loud — occupancy races resolve
// safe, anything else (permissions, I/O) wants the operator.
func pruneDir(dir string) (int, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if !fi.IsDir() {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var removed int
	for _, e := range entries {
		n, err := pruneDir(filepath.Join(dir, e.Name()))
		if err != nil {
			return removed, err
		}
		removed += n
	}
	if structural(fi.Name()) {
		return removed, nil
	}
	if err := os.Remove(dir); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, fs.ErrNotExist) {
			return removed, nil
		}
		return removed, err
	}
	return removed + 1, nil
}
