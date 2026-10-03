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
// symlinks, non-empty dirs, and root itself are never touched.
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

// pruneDir empties dir bottom-up and removes it when nothing
// remains, reporting how many dirs went in its subtree. A non-dir
// (file, symlink) is left alone; a dir that resists for any
// reason but occupancy or absence fails the run loud — occupancy
// races resolve safe, anything else (permissions, I/O) wants the
// operator.
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
	if err := os.Remove(dir); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, fs.ErrNotExist) {
			return removed, nil
		}
		return removed, err
	}
	return removed + 1, nil
}
