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
}

// Analyze walks the proven store root once, classifying by path
// shape under docker/registry/v2 (the layout
// docs/REGISTRY_LAYOUT.md pins): tag current/link files, revision
// links, repo _layers links, per-session _uploads dirs, blob data
// files with their sizes. Repo names nest, so only the tail is
// pinned — with the _manifests anchor where the layout fixes one.
// A nil token refuses before touching the disk; a missing root
// refuses; a root with no v2 tree yet reads as a fresh store
// (zeros); a v2 that is not a directory refuses. A walk error
// names its path — magnitude is exact or refused, never guessed.
func Analyze(store proof.FilesystemStore) (Report, error) {
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
	err = filepath.WalkDir(v2, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
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
					if k.IsDir() {
						rep.Uploads++
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
			switch {
			case n >= 7 && parts[n-2] == "current" && parts[n-4] == "tags" && parts[n-5] == "_manifests":
				rep.Tags++
			case n >= 7 && parts[n-4] == "revisions" && parts[n-5] == "_manifests":
				rep.Revisions++
			case parts[n-4] == "_layers":
				rep.LayerLinks++
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
