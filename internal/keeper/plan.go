package keeper

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// ManualReason marks hand-picked rows: plan add puts images into the
// plan without a policy behind them, and the reason says so on plan
// and in sweep activity.
const ManualReason = "manual"

// checkPatterns proves every pattern valid before a single mark
// lands: a typoed second pattern must not leave half a plan behind.
func checkPatterns(patterns []string) error {
	for _, p := range patterns {
		if _, err := policy.MatchImage(p, ""); err != nil {
			return err
		}
	}
	return nil
}

// matchAny reports whether the qualified name matches any pattern.
// Patterns are pre-checked, so a match error here is unreachable —
// treated as no match rather than a mid-plan failure.
func matchAny(patterns []string, qualified string) bool {
	for _, p := range patterns {
		if ok, err := policy.MatchImage(p, qualified); err == nil && ok {
			return true
		}
	}
	return false
}

// ListPlan returns pending candidates repo-major for stable output:
// the keep-n selector walks a map, so its order is random without
// this.
func ListPlan(ctx context.Context, s store.Store) ([]policy.Row, error) {
	due, err := s.Due(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis unreachable: %w", err)
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].Repo != due[j].Repo {
			return due[i].Repo < due[j].Repo
		}
		return due[i].Tag < due[j].Tag
	})
	return due, nil
}

// DiscardPlan drops every due mark, returning how many went. Rows
// survive; only marks go.
func DiscardPlan(ctx context.Context, s store.Store) (int, error) {
	n, err := s.ClearDue(ctx)
	if err != nil {
		return 0, fmt.Errorf("redis unreachable: %w", err)
	}
	return n, nil
}

// AddPlan marks tracked rows matching any pattern with a manual
// reason. A direct plan edit like discard: no dry-run, the operator
// named the images. Exact spellings (no wildcards, no regex: prefix)
// are typo-proof: one that matches no tracked row refuses before
// anything marks (MarkDue would conjure a phantom row the sweeper
// cannot resolve). Globs stay lenient — a pattern matching nothing
// just adds nothing. It returns how many rows marked.
func AddPlan(ctx context.Context, s store.Store, patterns []string) (int, error) {
	if err := checkPatterns(patterns); err != nil {
		return 0, err
	}
	rows, err := s.All(ctx)
	if err != nil {
		return 0, fmt.Errorf("redis unreachable: %w", err)
	}
	tracked := map[string]bool{}
	for _, r := range rows {
		tracked[r.Repo+":"+r.Tag] = true
	}
	for _, p := range patterns {
		if strings.HasPrefix(p, "regex:") {
			continue
		}
		if _, _, perr := policy.ParseExactImage(p); perr != nil {
			continue
		}
		if !tracked[p] {
			return 0, fmt.Errorf("image %s matches no tracked row: push it first or check the name", p)
		}
	}
	n := 0
	for _, r := range rows {
		if !matchAny(patterns, r.Repo+":"+r.Tag) {
			continue
		}
		if merr := s.MarkDue(ctx, r.Repo, r.Tag, ManualReason); merr != nil {
			return 0, fmt.Errorf("redis unreachable: %w", merr)
		}
		n++
	}
	return n, nil
}

// RemovePlan drops due marks matching any pattern — glob, regex:, or
// exact image, one matcher takes all three — returning how many went.
// No dry-run: a direct plan edit like discard.
func RemovePlan(ctx context.Context, s store.Store, patterns []string) (int, error) {
	if err := checkPatterns(patterns); err != nil {
		return 0, err
	}
	due, err := s.Due(ctx)
	if err != nil {
		return 0, fmt.Errorf("redis unreachable: %w", err)
	}
	n := 0
	for _, r := range due {
		if !matchAny(patterns, r.Repo+":"+r.Tag) {
			continue
		}
		ok, uerr := s.UnmarkDue(ctx, r.Repo, r.Tag)
		if uerr != nil {
			return 0, fmt.Errorf("redis unreachable: %w", uerr)
		}
		if ok {
			n++
		}
	}
	return n, nil
}
