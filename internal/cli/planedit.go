package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// manualReason marks hand-picked rows: plan add and reap add put
// images into the plan without a policy behind them, and the reason
// says so on plan and in sweep activity.
const manualReason = "manual"

// checkPatterns compiles nothing but proves every pattern valid
// before a single mark lands: a typoed second pattern must not leave
// half a plan behind.
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

// runPlanAdd marks tracked rows matching any pattern with a manual
// reason. A direct plan edit like discard: no dry-run, the operator
// named the images. Exact spellings (no wildcards, no regex: prefix)
// are typo-proof: one that matches no tracked row refuses before
// anything marks (MarkDue would conjure a phantom row the sweeper
// cannot resolve). Globs stay lenient — a pattern matching nothing
// just adds nothing.
func runPlanAdd(ctx context.Context, w io.Writer, s store.Store, patterns []string) error {
	if err := checkPatterns(patterns); err != nil {
		return err
	}
	rows, err := s.All(ctx)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
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
			return fmt.Errorf("image %s matches no tracked row: push it first or check the name", p)
		}
	}
	n := 0
	for _, r := range rows {
		if !matchAny(patterns, r.Repo+":"+r.Tag) {
			continue
		}
		if merr := s.MarkDue(ctx, r.Repo, r.Tag, manualReason); merr != nil {
			return fmt.Errorf("redis unreachable: %w", merr)
		}
		n++
	}
	if n == 0 {
		_, err := io.WriteString(w, "no tracked rows matched\n")
		return err
	}
	_, err = fmt.Fprintf(w, "marked %d rows due (%s)\n", n, manualReason)
	return err
}

// runPlanRemove drops due marks matching any pattern — glob, regex:,
// or exact image, one matcher takes all three. No dry-run: a direct
// plan edit like discard.
func runPlanRemove(ctx context.Context, w io.Writer, s store.Store, patterns []string) error {
	if err := checkPatterns(patterns); err != nil {
		return err
	}
	due, err := s.Due(ctx)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	n := 0
	for _, r := range due {
		if !matchAny(patterns, r.Repo+":"+r.Tag) {
			continue
		}
		ok, uerr := s.UnmarkDue(ctx, r.Repo, r.Tag)
		if uerr != nil {
			return fmt.Errorf("redis unreachable: %w", uerr)
		}
		if ok {
			n++
		}
	}
	if n == 0 {
		_, err := io.WriteString(w, "nothing removed\n")
		return err
	}
	_, err = fmt.Fprintf(w, "removed %d due marks\n", n)
	return err
}

var planAddCmd = &cobra.Command{
	Use:   "add <pattern>...",
	Short: "Mark tracked images matching patterns due",
	Long: `Mark tracked rows whose repo:tag (registry stripped) matches
any pattern, with a manual reason. Kyverno-style globs (* crosses
slashes, ? is one char) or regex: for full regex, repeatable —
matches union. An exact repo:tag spelling is typo-proof: matching
no tracked row refuses. A direct plan edit: no dry-run.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		s, err := OpenStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		return runPlanAdd(cmd.Context(), cmd.OutOrStdout(), s, args)
	},
}

var planRemoveCmd = &cobra.Command{
	Use:   "remove <pattern>...",
	Short: "Unmark due rows matching patterns",
	Long: `Drop due marks whose repo:tag (registry stripped) matches any
pattern: glob, regex:, or exact image, one matcher takes all three.
Rows survive; only marks go. A direct plan edit: no dry-run.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		s, err := OpenStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		return runPlanRemove(cmd.Context(), cmd.OutOrStdout(), s, args)
	},
}
