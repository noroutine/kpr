package cli

import (
	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"

	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/storeops"
)

var (
	adoptGen      string
	adoptNoDryRun bool
)

var adoptCmd = &cobra.Command{
	Use:   "adopt [IDENT]",
	Short: "Pair this store to the served lineage",
	Long: `Pair this store to the served lineage — or a pinned IDENT,
which must match it. Re-pairs a store paired elsewhere, untagging
the old epoch's served sentinel tags before pruning its rows.
Identity-less payloads refuse.`,
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
		armed := proof.Arm(adoptNoDryRun, config.Current().CLINoDryRun)
		return storeops.Adopt(cmd.Context(), cmd.OutOrStdout(), d.Reg, d.Store, d.Store, d.Reg, armed, ident, adoptGen)
	},
}

func init() {
	adoptCmd.Flags().StringVar(&adoptGen, "gen", "", "Served generation to accept as baseline (must match what the registry serves)")
	adoptCmd.Flags().BoolVar(&adoptNoDryRun, "no-dry-run", false, "Pair for real (default previews)")
	storeCmd.AddCommand(adoptCmd)
}
