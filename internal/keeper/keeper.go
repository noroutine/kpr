// Package keeper holds the keeper use cases: the operator intents
// (reap evaluation, plan, status) as plain functions over ports.
// Driving adapters (cli, web) parse input, call here, and render the
// result; nothing here knows about flags, handlers, or printing.
package keeper

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// CatalogSource is the outbound port evaluation consumes: the live
// tag list per repo. *registry.Client is the production adapter;
// tests bring a stub, never a loopback server.
type CatalogSource interface {
	Catalog(ctx context.Context, repo string) ([]string, error)
}

// PolicyNames are the reap selectors: every live policy plus all.
// expired takes elapsed TTL tags, partial takes digest-less stale
// uploads, untagged takes tags gone from the catalog past grace,
// keep-n takes everything past the freshest ten per repo.
var PolicyNames = []string{"all", "expired", "partial", "untagged", "keep-n"}

// fetchCatalogs reads the live tag list per tracked repo. A repo
// whose fetch fails stays out of the map, and catalog-dependent
// selectors treat absent as unknown (skip), never as empty.
func fetchCatalogs(ctx context.Context, reg CatalogSource, rows []policy.Row) map[string][]string {
	catalogs := map[string][]string{}
	if reg == nil {
		return catalogs
	}
	repos := map[string]bool{}
	for _, r := range rows {
		repos[r.Repo] = true
	}
	for repo := range repos {
		tags, cerr := reg.Catalog(ctx, repo)
		if cerr != nil {
			continue
		}
		catalogs[repo] = tags
	}
	return catalogs
}

// evalOne runs a single named policy over rows. Unknown names refuse
// before anything marks, so a typo never reaps the world.
func evalOne(name string, rows []policy.Row, catalogs map[string][]string, now time.Time, keepNExclude []string) ([]policy.Row, error) {
	switch name {
	case "expired":
		return policy.SelectExpired(rows, now), nil
	case "partial":
		return policy.SelectStaleUploads(rows, now), nil
	case "untagged":
		return policy.SelectUntagged(rows, catalogs, now), nil
	case "keep-n":
		return policy.SelectKeepN(rows, policy.KeepN, nil, keepNExclude, now), nil
	default:
		return nil, fmt.Errorf("unknown policy %q (want one of: %s)", name, strings.Join(PolicyNames, ", "))
	}
}

// sortMarks orders marks repo-major for stable plan output: the
// keep-n selector walks a map, so its order is random without this.
func sortMarks(out []policy.Row) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Tag < out[j].Tag
	})
}

// EvaluatePolicy runs one named policy (or all) over tracked rows
// plus live catalogs. keepNExclude spares keep-N for rows whose
// repo:tag matches (registry stripped). Exported alongside
// EvaluatePolicies so scripts drive one policy path, never a copy.
func EvaluatePolicy(ctx context.Context, s store.Store, reg CatalogSource, now time.Time, keepNExclude []string, name string) ([]policy.Row, error) {
	if name == "all" {
		return EvaluatePolicies(ctx, s, reg, now, keepNExclude)
	}
	rows, err := s.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis unreachable: %w", err)
	}
	var catalogs map[string][]string
	if name == "untagged" {
		catalogs = fetchCatalogs(ctx, reg, rows)
	}
	marked, err := evalOne(name, rows, catalogs, now, keepNExclude)
	if err != nil {
		return nil, err
	}
	sortMarks(marked)
	return marked, nil
}

// EvaluatePolicies runs every policy over tracked rows plus live
// catalogs and returns the joined mark per row. Catalog failures skip
// that repo's catalog-dependent selectors (rows-only selectors still
// apply). keepNExclude spares keep-N for rows whose repo:tag matches
// (registry stripped). Exported so the e2e scenarios (test/e2e) drive
// the same evaluation the CLI marks from — one policy path, never a
// copy.
func EvaluatePolicies(ctx context.Context, s store.Store, reg CatalogSource, now time.Time, keepNExclude []string) ([]policy.Row, error) {
	rows, err := s.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis unreachable: %w", err)
	}
	catalogs := fetchCatalogs(ctx, reg, rows)
	marks := map[string][]string{}
	for _, name := range []string{"expired", "partial", "untagged", "keep-n"} {
		marked, serr := evalOne(name, rows, catalogs, now, keepNExclude)
		if serr != nil {
			return nil, serr
		}
		for _, r := range marked {
			k := r.Repo + "\x00" + r.Tag
			marks[k] = append(marks[k], r.Reason)
		}
	}

	var out []policy.Row
	for _, r := range rows {
		if reasons, ok := marks[r.Repo+"\x00"+r.Tag]; ok {
			r.Due = true
			r.Reason = strings.Join(reasons, "; ")
			out = append(out, r)
		}
	}
	sortMarks(out)
	return out, nil
}
