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

var planRemoveCmd = &cobra.Command{
	Use:   "remove <pattern>...",
	Short: "Unmark due rows matching patterns",
	Long: `Drop due marks whose repo:tag matches any pattern. Rows survive;
only marks go.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		s := d.Store
		return runPlanRemove(cmd.Context(), cmd.OutOrStdout(), s, args)
	},
}
