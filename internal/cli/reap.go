package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// runReap renders the reap verdict: the plan in both modes —
// rows read identically dry or armed, only the trailer says
// whether they were marked. Evaluation and marking live in
// keeper.Reap; this stays printing-only.
func runReap(ctx context.Context, w io.Writer, s store.Store, reg keeper.CatalogSource, armed proof.ArmedRun, excludes []string, now time.Time, policyName string) error {
	marked, err := keeper.Reap(ctx, s, reg, now, excludes, policyName, armed)
	if err != nil {
		return err
	}
	if len(marked) == 0 {
		if !proof.Unarmed(armed) {
			_, err := io.WriteString(w, "marked 0 rows due\n")
			return err
		}
		_, err := io.WriteString(w, "nothing due (dry-run)\n")
		return err
	}
	for _, r := range marked {
		if _, err := fmt.Fprintf(w, "%s:%s — %s\n", r.Repo, r.Tag, r.Reason); err != nil {
			return err
		}
	}
	if !proof.Unarmed(armed) {
		_, err = fmt.Fprintf(w, "marked %d rows due\n", len(marked))
		return err
	}
	_, err = io.WriteString(w, "(dry-run: nothing marked; re-run with --no-dry-run to mark)\n")
	return err
}

var (
	reapNoDryRun bool
	reapExclude  []string
)

var reapCmd = &cobra.Command{
	Use:   "reap [policy]",
	Short: "Evaluate policies and mark rows due",
	Long: `Evaluate one policy (or all) and mark selected rows due with
reasons. Policies: ttl, hash, partial, untagged, keep-n.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		cfg, s := d.Cfg, d.Store
		armed := proof.Arm(reapNoDryRun, cfg.CLINoDryRun)
		name := "all"
		if len(args) == 1 {
			name = args[0]
		}
		return runReap(cmd.Context(), cmd.OutOrStdout(), s,
			d.Reg, armed, reapExclude, time.Now().UTC(), name)
	},
}

func init() {
	reapCmd.Flags().BoolVar(&reapNoDryRun, "no-dry-run", false, "Mark rows due for real (default prints the plan only)")
	reapCmd.Flags().StringSliceVar(&reapExclude, "exclude", nil, "Spare keep-N for rows whose repo:tag matches (repeatable regex, registry stripped)")
	RootCmd.AddCommand(reapCmd)
}
