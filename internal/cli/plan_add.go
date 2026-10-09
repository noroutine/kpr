package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
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

var planAddCmd = &cobra.Command{
	Use:   "add <pattern>...",
	Short: "Mark tracked images matching patterns due",
	Long: `Mark tracked rows whose repo:tag matches any pattern — globs,
regex:, or exact spellings. An exact spelling matching nothing
refuses.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		s := d.Store
		return runPlanAdd(cmd.Context(), cmd.OutOrStdout(), s, args)
	},
}
