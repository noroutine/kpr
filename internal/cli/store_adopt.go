package cli

import (
	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"

	"nrtn.dev/catalyst/kpr/internal/storeops"
)

var adoptGen string

var adoptCmd = &cobra.Command{
	Use:   "adopt [IDENT]",
	Short: "Pair this store to the served lineage",
	Long: `Pair this store to the served lineage — or a pinned IDENT,
which must match it. Re-pairs a store paired elsewhere, pruning
the old epoch's sentinel rows. Identity-less payloads refuse.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		ident := ""
		if len(args) == 1 {
			ident = args[0]
		}
		return storeops.Adopt(cmd.Context(), cmd.OutOrStdout(), d.Reg, d.Store, d.Store, ident, adoptGen)
	},
}

func init() {
	adoptCmd.Flags().StringVar(&adoptGen, "gen", "", "Served generation to accept as baseline (must match what the registry serves)")
	storeCmd.AddCommand(adoptCmd)
}
