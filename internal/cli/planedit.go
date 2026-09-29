package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// runPlanAdd reports the add count. The marks land in keeper.AddPlan;
// this stays reporting-only.
func runPlanAdd(ctx context.Context, w io.Writer, s store.Store, patterns []string) error {
	n, err := keeper.AddPlan(ctx, s, patterns)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err := io.WriteString(w, "no tracked rows matched\n")
		return err
	}
	_, err = fmt.Fprintf(w, "marked %d rows due (%s)\n", n, keeper.ManualReason)
	return err
}

// runPlanRemove reports the remove count. The marks drop in
// keeper.RemovePlan; this stays reporting-only.
func runPlanRemove(ctx context.Context, w io.Writer, s store.Store, patterns []string) error {
	n, err := keeper.RemovePlan(ctx, s, patterns)
	if err != nil {
		return err
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
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		s := d.store
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
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		s := d.store
		return runPlanRemove(cmd.Context(), cmd.OutOrStdout(), s, args)
	},
}
