// Package registryfs walks a filesystem registry store: the
// magnitude numbers (how many repos, tags, revisions, blobs, and
// bytes) that tell whether a registry is small enough to reason
// about by hand. It takes the sealed filestore proof, never a
// bare path — no proof, no walk. Read-only and verdict-free:
// gaps between the counts (tags without revisions and the like)
// are reported as raw numbers for later detectors to interpret,
// never judged here.
package registryfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"nrtn.dev/catalyst/kpr/internal/proof"
)

// Report is one walk's magnitude: counts per file kind plus blob
// bytes. Repos counts dirs holding a _manifests child (nested
// names count once, at their own depth); it is the fs-side
// catalog size.
type Report struct {
	Repos      int
	Tags       int
	Revisions  int
	LayerLinks int
	Uploads    int
	Blobs      int
	BlobBytes  int64
	// LinkBytes weighs the counted link files (tag, revision,
	// layer membership) — the registry's own index.
	LinkBytes int64
	// UploadBytes weighs upload session files (startedAt,
	// hashstates, partial data) — in-flight cargo and residue,
	// neither blob nor index.
	UploadBytes int64
}

// Analyze walks the proven store root, classifying by path shape
// under docker/registry/v2 (the layout docs/REGISTRY_LAYOUT.md
// pins): tag current/link files, revision links, repo _layers
// links, per-session _uploads dirs, blob data files with their
// sizes. Repo names nest, so only the tail is pinned — with the
// _manifests anchor where the layout fixes one. The two subtrees
// (repositories, blobs) walk in parallel — they never overlap,
// and the shards merge at the join. Progress reports the running
// merged report every 1024 visits per shard plus once at the end
// (nil skips it) — the caller throttles rendering; middles may
// arrive out of order, the end is exact. A nil token refuses
// before touching the disk; a missing root refuses; a root with
// no v2 tree yet reads as a fresh store (zeros); a v2 that is not
// a directory refuses. A walk error names its path — magnitude is
// exact or refused, never guessed.
func Analyze(store proof.FilesystemStore, progress func(Report)) (Report, error) {
	var rep Report
	if store == nil {
		return rep, fmt.Errorf("analyze without filestore proof: refusing to walk blind (prove the registry config first)")
	}
	rootInfo, err := os.Stat(store.Root())
	if err != nil {
		return rep, fmt.Errorf("analyze %s: %w", store.Root(), err)
	}
	if !rootInfo.IsDir() {
		return rep, fmt.Errorf("analyze %s: not a directory", store.Root())
	}
	v2 := filepath.Join(store.Root(), "docker", "registry", "v2")
	v2Info, err := os.Stat(v2)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, fmt.Errorf("analyze %s: %w", v2, err)
	}
	if !v2Info.IsDir() {
		return rep, fmt.Errorf("analyze %s: not a directory", v2)
	}
	shards := []shard{{name: "repositories"}, {name: "blobs"}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := walkShard(v2, shards[i].name, func(running Report) {
				mu.Lock()
				shards[i].rep = running
				total := shards[0].rep
				total.add(shards[1].rep)
				mu.Unlock()
				if progress != nil {
					progress(total)
				}
			})
			mu.Lock()
			shards[i].rep = rep
			shards[i].err = err
			mu.Unlock()
		}()
	}
	wg.Wait()
	for i := range shards {
		if shards[i].err != nil {
			return rep, shards[i].err
		}
	}
	total := shards[0].rep
	total.add(shards[1].rep)
	if progress != nil {
		progress(total)
	}
	return total, nil
}

// shard is one walker's partial result: rep accrues under mu,
// err is written once by its goroutine and read after the join.
type shard struct {
	name string
	rep  Report
	err  error
}

// add folds another report in, field by field.
func (r *Report) add(o Report) {
	r.Repos += o.Repos
	r.Tags += o.Tags
	r.Revisions += o.Revisions
	r.LayerLinks += o.LayerLinks
	r.Uploads += o.Uploads
	r.Blobs += o.Blobs
	r.BlobBytes += o.BlobBytes
	r.LinkBytes += o.LinkBytes
	r.UploadBytes += o.UploadBytes
}

// walkShard walks one v2 subtree, classifying exactly as the old
// single walk did — the shard root only narrows where WalkDir
// starts, path shapes still anchor on v2. A missing shard reads
// as zeros (blobs can land before any repo exists, and vice
// versa); anything else failing names its path.
func walkShard(v2, name string, progress func(Report)) (Report, error) {
	var rep Report
	dir := filepath.Join(v2, name)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, fmt.Errorf("analyze %s: %w", dir, err)
	}
	visits := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		visits++
		if progress != nil && visits%1024 == 0 {
			progress(rep)
		}
		rel, rerr := filepath.Rel(v2, path)
		if rerr != nil {
			return rerr
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if d.IsDir() {
			// Tags may start with an underscore (repo components
			// may not), so a tag named _uploads or _manifests
			// must not classify as an upload area or a repo:
			// dir classification stops below _manifests. The
			// tag's own current/link still counts through the
			// file branch below.
			if len(parts) >= 2 && slices.Contains(parts[1:len(parts)-1], "_manifests") {
				return nil
			}
			switch {
			case len(parts) >= 2 && parts[0] == "repositories" && parts[len(parts)-1] == "_uploads":
				// Per-repo upload sessions: one subdir each.
				// Counted here, never descended into.
				kids, kerr := os.ReadDir(path)
				if kerr != nil {
					return kerr
				}
				for _, k := range kids {
					if !k.IsDir() {
						continue
					}
					rep.Uploads++
					files, ferr := os.ReadDir(filepath.Join(path, k.Name()))
					if ferr != nil {
						return ferr
					}
					for _, f := range files {
						if f.IsDir() {
							continue
						}
						fi, ierr := f.Info()
						if ierr != nil {
							return ierr
						}
						rep.UploadBytes += fi.Size()
					}
				}
				return fs.SkipDir
			case len(parts) >= 2 && parts[0] == "repositories":
				// A repo is a dir holding _manifests, at whatever
				// depth nesting puts it.
				if mi, merr := os.Stat(filepath.Join(path, "_manifests")); merr == nil && mi.IsDir() {
					rep.Repos++
				} else if merr != nil && !os.IsNotExist(merr) {
					return merr
				}
			}
			return nil
		}
		if !d.IsDir() && filepath.Base(path) == "link" && len(parts) >= 6 && parts[0] == "repositories" {
			// Suffix shapes (repo names nest, so only the tail
			// is pinned, anchored on _manifests where the
			// layout fixes one): tags end in
			// _manifests/tags/<tag>/current/link, revisions in
			// _manifests/revisions/<algo>/<hex>/link, layer
			// memberships in <repo>/_layers/<algo>/<hex>/link.
			// Index links (…/index/…) match none and stay
			// uncounted, whatever the algorithm names.
			n := len(parts)
			counted := false
			switch {
			case n >= 7 && parts[n-2] == "current" && parts[n-4] == "tags" && parts[n-5] == "_manifests":
				rep.Tags++
				counted = true
			case n >= 7 && parts[n-4] == "revisions" && parts[n-5] == "_manifests":
				rep.Revisions++
				counted = true
			case parts[n-4] == "_layers":
				rep.LayerLinks++
				counted = true
			}
			if counted {
				fi, ferr := d.Info()
				if ferr != nil {
					return ferr
				}
				rep.LinkBytes += fi.Size()
			}
		}
		if !d.IsDir() && len(parts) >= 2 && parts[0] == "blobs" && filepath.Base(path) == "data" {
			rep.Blobs++
			fi, serr := d.Info()
			if serr != nil {
				return serr
			}
			rep.BlobBytes += fi.Size()
		}
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("analyze %s: %w", v2, err)
	}
	return rep, nil
}
