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

// Prober is the outbound port status consumes: registry reachability
// for the banner. Same production adapter, same stub rule.
type Prober interface {
	Reachable(ctx context.Context) error
}

// PlanEntry is one due row with its reason; ActivityEntry is one
// resolved row. Times stay raw — adapters format for humans.
type PlanEntry struct {
	Repo     string
	Tag      string
	Reason   string
	PushedAt time.Time
}

// ActivityEntry is one resolved row: what the pass did, who did it,
// what caused it, and when.
type ActivityEntry struct {
	Repo    string
	Tag     string
	Reason  string
	Outcome string
	At      time.Time
	Actor   string
	Trigger string
}

// Status is everything the console and `status` show about what kpr
// tracks: backend health, counters, plan, activity — never a registry
// catalog. One counting implementation serves both.
type Status struct {
	StoreOK    bool
	RegistryOK bool
	Tracked    int
	Due        int
	Performed  int
	Planned    int
	Failed     int
	Untracked  int
	Plan       []PlanEntry
	Activity   []ActivityEntry
	Current    store.Current
}

// FetchStatus reads the tracked state, degrading to red/empty when a
// backend is absent or down. Adapters own the policy: the CLI fails
// on a down store, the console renders the red banner.
func FetchStatus(ctx context.Context, s store.Store, reg Prober) Status {
	var st Status
	if s != nil {
		st.StoreOK = s.Ping(ctx) == nil
	}
	if reg != nil {
		st.RegistryOK = reg.Reachable(ctx) == nil
	}
	if s == nil || !st.StoreOK {
		return st
	}
	rows, err := s.All(ctx)
	if err != nil {
		st.StoreOK = false
		return st
	}
	// NOTE(mutants): <= is equivalent on both legs — repo:tag pairs
	// are unique, so equal elements never compare and the order is
	// total either way.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		return rows[i].Tag < rows[j].Tag
	})
	st.Tracked = len(rows)
	for _, r := range rows {
		if !r.Due {
			continue
		}
		st.Due++
		st.Plan = append(st.Plan, PlanEntry{Repo: r.Repo, Tag: r.Tag, Reason: r.Reason, PushedAt: r.PushedAt})
	}
	cur, _ := s.GetCurrent(ctx)
	st.Current = cur
	acts, err := s.Activity(ctx)
	if err != nil {
		return st
	}
	for _, a := range acts {
		switch a.Outcome {
		case "deleted":
			st.Performed++
		case "planned":
			st.Planned++
		case "failed":
			st.Failed++
		case "untracked":
			st.Untracked++
		}
		st.Activity = append(st.Activity, ActivityEntry{
			Repo: a.Repo, Tag: a.Tag, Reason: a.Reason, Outcome: a.Outcome, At: a.At,
			Actor: a.Actor, Trigger: a.Trigger,
		})
	}
	return st
}

// PolicyNames are the reap selectors: every live policy plus all.
// ttl takes elapsed explicit TTLs (bare numbers, suffixed tags),
// hash takes bare commit hashes past the hash default, partial
// takes digest-less stale uploads, untagged takes tags gone from
// the catalog past grace, keep-n takes everything past the
// freshest ten per repo.
var PolicyNames = []string{"all", "ttl", "hash", "partial", "untagged", "keep-n"}

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
	case "ttl":
		return policy.SelectTTL(rows, now), nil
	case "hash":
		return policy.SelectHashes(rows, now), nil
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
	// NOTE(mutants): <= is equivalent on both legs — repo:tag pairs
	// are unique, so equal elements never compare and the order is
	// total either way.
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
	for _, name := range []string{"ttl", "hash", "partial", "untagged", "keep-n"} {
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

// Reap evaluates one policy (or all) and, when armed, marks the rows
// due. Marks accumulate across calls until sweep or plan discard: a
// second reap adds its rows, never wipes the first policy's. Unarmed
// it only evaluates (the caller prints the plan): dry-run is implicit,
// --no-dry-run explicit. excludes spares keep-N for matching repo:tag
// names. It returns the evaluated rows either way, marked or not.
func Reap(ctx context.Context, s store.Store, reg CatalogSource, now time.Time, keepNExclude []string, policyName string, armed bool) ([]policy.Row, error) {
	marked, err := EvaluatePolicy(ctx, s, reg, now, keepNExclude, policyName)
	if err != nil {
		return nil, err
	}
	if !armed {
		return marked, nil
	}
	for _, r := range marked {
		if merr := s.MarkDue(ctx, r.Repo, r.Tag, r.Reason); merr != nil {
			return nil, fmt.Errorf("redis unreachable: %w", merr)
		}
	}
	return marked, nil
}
