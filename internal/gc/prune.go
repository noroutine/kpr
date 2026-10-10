package gc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// pruneEmptyDirs removes the collector's leftover directory
// skeleton under root, deepest first: the stock collector deletes
// blobs and links but never their parent dirs, so every gc leaves
// an empty tree behind. Only genuinely empty directories go —
// os.Remove is the emptiness check and the delete in one atomic
// step, so a dir that gains a file mid-run survives. Files,
// symlinks, non-empty dirs, structural skeleton containers (which
// the stock walker stats whether or not they hold anything), and
// root itself are never touched: prune stays invisible to the next
// collect.
func pruneEmptyDirs(root string) (int, error) {
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

// planPrune counts what pruneEmptyDirs would remove on this
// state, removing nothing: the preview half of the prune rule.
// Both halves share structural and the bottom-up emptiness
// reading; the agreement test pins them together on staged
// trees, so the preview count is the armed count.
func planPrune(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("skeleton unreadable at %s: %w", root, err)
	}
	var n int
	for _, e := range entries {
		c, _, err := planDir(filepath.Join(root, e.Name()))
		if err != nil {
			return n, err
		}
		n += c
	}
	return n, nil
}

// planDir reports the removals in dir's subtree and whether dir
// itself would go: a dir goes when it holds nothing but gone
// children (empty counts as vacuous). Files, symlinks, and
// structural containers stay — matching pruneDir's removals on a
// quiet state. An absent dir reads gone for neither side: armed
// skips it uncounted, so the plan counts nothing for it either.
func planDir(dir string) (int, bool, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !fi.IsDir() {
		return 0, false, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false, err
	}
	var n int
	allGone := true
	for _, e := range entries {
		c, gone, err := planDir(filepath.Join(dir, e.Name()))
		if err != nil {
			return n, false, err
		}
		n += c
		if !gone {
			allGone = false
		}
	}
	if structural(fi.Name()) {
		return n, false, nil
	}
	if allGone {
		return n + 1, true, nil
	}
	return n, false, nil
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
