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

// runDiscardPlan reports the discard count. No dry-run: discarding
// previews nothing — plan already showed the rows. The marks drop in
// keeper.DiscardPlan; this stays reporting-only.
func runDiscardPlan(ctx context.Context, w io.Writer, s store.Store) error {
	n, err := keeper.DiscardPlan(ctx, s)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err := io.WriteString(w, "nothing due\n")
		return err
	}
	_, err = fmt.Fprintf(w, "discarded %d due marks\n", n)
	return err
}

var planDiscardCmd = &cobra.Command{
	Use:   "discard",
	Short: "Drop the whole plan (clear all due marks)",
	Long:  `Clear every due mark. Rows survive; only marks go.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		s := d.Store
		return runDiscardPlan(cmd.Context(), cmd.OutOrStdout(), s)
	},
}
