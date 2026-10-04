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

// ListHusks names tagless repos without the full walk: one entry
// per repo dir holding _manifests with no tag current/link,
// sorted, sentinel machinery excluded. Subtrees that hold neither
// repos nor tags (_layers, _uploads, revisions) are skipped, so a
// listing stays seconds where a full analyze reads every blob.
// The tag shape and the sentinel split match the full walk
// exactly — TestListHusksMatchesAnalyze pins the agreement. It
// takes the sealed filestore proof, never a bare path.
func ListHusks(store proof.FilesystemStore) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("husk listing without filestore proof: refusing to walk blind (prove the registry config first)")
	}
	rootInfo, err := os.Stat(store.Root())
	if err != nil {
		return nil, fmt.Errorf("husks %s: %w", store.Root(), err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("husks %s: not a directory", store.Root())
	}
	v2 := filepath.Join(store.Root(), "docker", "registry", "v2")
	if _, err := os.Stat(v2); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("husks %s: %w", v2, err)
	}
	repos := filepath.Join(v2, "repositories")
	if _, err := os.Stat(repos); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("husks %s: %w", repos, err)
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
