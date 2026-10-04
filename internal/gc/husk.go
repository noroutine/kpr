package gc

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"nrtn.dev/catalyst/kpr/internal/backfill"
)

// RemoveHusks deletes tagless repo dirs under the registry root:
// nothing pullable lives there, so removal only orphans blobs the
// next collect owns (shared layers stay alive through other repos'
// links). Each candidate is re-verified at removal time — no tag
// links, no live upload session, never a sentinel-prefix repo, and
// never an ancestor of a kept repo — so a mid-run push races safe:
// anything gained since the walk keeps the repo. Names come back
// sorted for the report. A walk failure or a removal failure
// refuses loud: half-removed inventory must shout, never guess.
func RemoveHusks(root string) ([]string, error) {
	repos := filepath.Join(root, "docker", "registry", "v2", "repositories")
	if _, err := os.Stat(repos); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("husk walk %s: %w", repos, err)
	}
	var husks, kept []string
	var collect func(dir, repo string) error
	collect = func(dir, repo string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			name := e.Name()
			if repo != "" {
				name = repo + "/" + name
			}
			mi, merr := os.Stat(filepath.Join(sub, "_manifests"))
			if merr == nil && mi.IsDir() {
				kind, kerr := classify(sub, name)
				if kerr != nil {
					return kerr
				}
				if kind == keptRepo {
					kept = append(kept, name)
				} else {
					husks = append(husks, name)
				}
			} else if merr != nil && !os.IsNotExist(merr) {
				return merr
			}
			// Always descend: a tagged repo may nest under a
			// husk, and the removal guard below needs to see
			// it to spare the ancestor.
			if err := collect(sub, name); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(repos, ""); err != nil {
		return nil, fmt.Errorf("husk walk %s: %w", repos, err)
	}
	// Sorted removal: a parent sorts before its children, so a
	// husk gone with its ancestor is skipped, never re-removed.
	slices.Sort(husks)
	var removed []string
outer:
	for _, name := range husks {
		for _, k := range kept {
			if strings.HasPrefix(k, name+"/") {
				continue outer
			}
		}
		for _, r := range removed {
			if strings.HasPrefix(name, r+"/") {
				continue outer
			}
		}
		if err := os.RemoveAll(filepath.Join(repos, filepath.FromSlash(name))); err != nil {
			return removed, fmt.Errorf("husk remove %s: %w", name, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// repoKind is what a _manifests dir turned out to be: kept (tags,
// live uploads, or machinery) or husk (tagless, idle, removable).
type repoKind int

const (
	huskRepo repoKind = iota
	keptRepo
)

// classify re-verifies one candidate at removal time: a tag link
// since the walk, a live upload session, or a sentinel prefix each
// keep. A walk that cannot read refuses instead of assuming empty —
// unreadable is unknown, never tagless.
func classify(dir, repo string) (repoKind, error) {
	if strings.HasPrefix(repo, backfill.SentinelPrefix) {
		return keptRepo, nil
	}
	var tagged, busy bool
	if err := filepath.WalkDir(filepath.Join(dir, "_manifests", "tags"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "link" && filepath.Base(filepath.Dir(path)) == "current" {
			tagged = true
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return keptRepo, err
	}
	if err := filepath.WalkDir(filepath.Join(dir, "_uploads"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != filepath.Join(dir, "_uploads") {
			busy = true
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return keptRepo, err
	}
	if tagged || busy {
		return keptRepo, nil
	}
	return huskRepo, nil
}
