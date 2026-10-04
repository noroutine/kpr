package registryfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// repositoriesDir resolves the v2 repositories dir off the sealed
// proof: empty (no error) when the layout is absent — a fresh volume
// holds no repos, which is a view, not a failure. A bare path never
// reaches here; the proof carries the root.
func repositoriesDir(store proof.FilesystemStore, what string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("%s without filestore proof: refusing to walk blind (prove the registry config first)", what)
	}
	rootInfo, err := os.Stat(store.Root())
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", what, store.Root(), err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("%s %s: not a directory", what, store.Root())
	}
	v2 := filepath.Join(store.Root(), "docker", "registry", "v2")
	if _, err := os.Stat(v2); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("%s %s: %w", what, v2, err)
	}
	repos := filepath.Join(v2, "repositories")
	if _, err := os.Stat(repos); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("%s %s: %w", what, repos, err)
	}
	return repos, nil
}

// ListRepos names every repo holding a _manifests dir — the reap
// second opinion for catalog-unknown rows: catalog-unknown plus
// fs-absent means deleted, catalog-unknown plus fs-present means a
// catalog read failure. Tagged or not, sentinel machinery included:
// the caller decides what machinery means, this only reports the fs
// view. One walk, no tag reads, so it stays seconds where a full
// analyze reads every blob. It takes the sealed filestore proof,
// never a bare path.
func ListRepos(store proof.FilesystemStore) (map[string]bool, error) {
	repos, err := repositoriesDir(store, "repo listing")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if repos == "" {
		return out, nil
	}
	err = filepath.WalkDir(repos, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.IsDir() {
			return nil
		}
		// Dir classification stops below _manifests, exactly like
		// the full walk (analyze.go): manifest data never reads as
		// a repo, hostile tag names included. Recorded repos stay
		// open: a nested repo beside a _manifests dir still
		// classifies on the way down.
		switch d.Name() {
		case "_layers", "_uploads", "revisions", "_manifests":
			return fs.SkipDir
		}
		if mi, merr := os.Stat(filepath.Join(path, "_manifests")); merr == nil && mi.IsDir() {
			rel, rerr := filepath.Rel(repos, path)
			if rerr != nil {
				return rerr
			}
			out[filepath.ToSlash(rel)] = true
		} else if merr != nil && !os.IsNotExist(merr) {
			return merr
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("repo listing %s: %w", repos, err)
	}
	return out, nil
}

// ListHusks names tagless repos without the full walk: one entry
// per repo dir holding _manifests with no tag current/link,
// sorted, sentinel machinery excluded. Subtrees that hold neither
// repos nor tags (_layers, _uploads, revisions) are skipped, so a
// listing stays seconds where a full analyze reads every blob.
// The tag shape and the sentinel split match the full walk
// exactly — TestListHusksMatchesAnalyze pins the agreement. It
// takes the sealed filestore proof, never a bare path.
func ListHusks(store proof.FilesystemStore) ([]string, error) {
	repos, err := repositoriesDir(store, "husks")
	if err != nil {
		return nil, err
	}
	if repos == "" {
		return nil, nil
	}
	var names, dirs []string
	err = filepath.WalkDir(repos, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			case "_layers", "_uploads", "revisions":
				return fs.SkipDir
			}
			if mi, merr := os.Stat(filepath.Join(path, "_manifests")); merr == nil && mi.IsDir() {
				rel, rerr := filepath.Rel(repos, path)
				if rerr != nil {
					return rerr
				}
				name := filepath.ToSlash(rel)
				if !strings.Contains(name, "_manifests") && !strings.HasPrefix(name, backfill.SentinelPrefix) {
					names = append(names, name)
					dirs = append(dirs, path)
				}
			} else if merr != nil && !os.IsNotExist(merr) {
				return merr
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("husks %s: %w", repos, err)
	}
	var husks []string
	for i, dir := range dirs {
		tagged := false
		terr := filepath.WalkDir(filepath.Join(dir, "_manifests", "tags"), func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if !d.IsDir() && d.Name() == "link" && filepath.Base(filepath.Dir(path)) == "current" {
				tagged = true
			}
			return nil
		})
		if terr != nil && !os.IsNotExist(terr) {
			return nil, fmt.Errorf("husks %s: %w", dir, terr)
		}
		if !tagged {
			husks = append(husks, names[i])
		}
	}
	slices.Sort(husks)
	return husks, nil
}
