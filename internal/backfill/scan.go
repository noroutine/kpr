package backfill

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// CatalogReport is what the registry API sees: repos the catalog
// lists, tags their lists name, sentinel tags apart (machinery
// counts on both sides of the comparison), and what failed to
// enumerate. The API cannot see revisions, blobs, uploads, or
// layer links — there are no endpoints for them — so an fs/API
// comparison covers repos and tags only, and the rest stays
// fs-side.
type CatalogReport struct {
	Repos      int
	Tags       int
	Sentinels  int
	Husks      int
	FailedTags int
	// Prime annotates the sentinel prime: the floater is a
	// pointer, never inventory, so a seen prime leaves both
	// counts. Present is structure; missing and corrupt call
	// for investigation.
	Prime sentinel.Prime
}

// ScanCatalog walks the catalog the way backfill does — every repo,
// every tag list — counting what the API names. Progress reports
// the running counts after every repo (nil skips it). A failed
// catalog refuses the run; a 404 tag list is a husk, counted
// apart and never warned. Warnings go to w; the stream stays
// silent.
func ScanCatalog(ctx context.Context, w io.Writer, reg Registry, progress func(CatalogReport)) (CatalogReport, error) {
	rep := CatalogReport{Prime: sentinel.PrimeMissing}
	report := func() {
		if progress != nil {
			progress(rep)
		}
	}
	repos, err := reg.CatalogAll(ctx)
	if err != nil {
		return rep, fmt.Errorf("catalog enumeration: %w", err)
	}
	for _, repo := range repos {
		// No sentinel skip: this counts what the API names, and
		// the fs walk counts machinery too — the comparison only
		// holds when both sides see everything. (Run adopts them
		// too, capped at the floater's push; counting is still
		// not adopting.)
		tags, err := reg.Catalog(ctx, repo)
		if err != nil && !isNotFound(err) {
			return rep, fmt.Errorf("catalog tags for %s: %w", repo, err)
		}
		if len(tags) == 0 {
			// A 404 tags/list reads tagless, and so does an
			// empty list: nothing to count either way.
			rep.Husks++
			report()
			continue
		}
		rep.Repos++
		rep.Tags += len(tags)
		if strings.HasPrefix(repo, SentinelPrefix) {
			rep.Sentinels += len(tags)
		}
		if repo == sentinel.Repo && slices.Contains(tags, sentinel.Tag) {
			// Seen prime leaves both counts: one generation,
			// two tags, and the pointer is the duplicate.
			// One manifest read says whether it resolves.
			rep.Sentinels--
			rep.Tags--
			if _, _, derr := reg.ManifestDigest(ctx, repo, sentinel.Tag); derr != nil {
				rep.Prime = sentinel.PrimeCorrupt
			} else {
				rep.Prime = sentinel.PrimePresent
			}
		}
		report()
	}
	return rep, nil
}
