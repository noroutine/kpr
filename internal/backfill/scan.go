package backfill

import (
	"context"
	"fmt"
	"io"
	"strings"
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
}

// ScanCatalog walks the catalog the way backfill does — every repo,
// every tag list — counting what the API names. Progress reports
// the running counts after every repo (nil skips it). A failed
// catalog refuses the run; a 404 tag list is a husk, counted
// apart and never warned. Warnings go to w; the stream stays
// silent.
func ScanCatalog(ctx context.Context, w io.Writer, reg Registry, progress func(CatalogReport)) (CatalogReport, error) {
	var rep CatalogReport
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
		if err != nil {
			if isNotFound(err) {
				// A 404 tags/list is a husk — tagless, nothing
				// to count — not a vanish: counted apart,
				// never warned per repo.
				rep.Husks++
				report()
				continue
			}
			return rep, fmt.Errorf("catalog tags for %s: %w", repo, err)
		}
		rep.Repos++
		rep.Tags += len(tags)
		if strings.HasPrefix(repo, SentinelPrefix) {
			rep.Sentinels += len(tags)
		}
		report()
	}
	return rep, nil
}
