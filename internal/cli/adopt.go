package cli

import (
	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/clideps"

	"nrtn.dev/catalyst/kpr/internal/gc"
)

var adoptGen string

var adoptCmd = &cobra.Command{
	Use:   "adopt [IDENT]",
	Short: "Pair this store to the served lineage",
	Long: `Pair this store to the lineage the registry serves, without
minting. The explicit ceremony for every pairing the verdict
refuses to do on its own: an unpaired store follows the served
identity (or a pinned IDENT, which must match it); a store paired
elsewhere re-pairs and prunes the old epoch's sentinel rows;
--gen names the served generation, accepting a rollback as
baseline. Identity-less payloads refuse even here: wipe the
volume or remove the stale tags instead. At most one IDENT.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := clideps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		ident := ""
		if len(args) == 1 {
			ident = args[0]
		}
		return gc.Adopt(cmd.Context(), cmd.OutOrStdout(), d.Reg, d.Store, d.Store, ident, adoptGen)
	},
}

func init() {
	adoptCmd.Flags().StringVar(&adoptGen, "gen", "", "Served generation to accept as baseline (must match what the registry serves)")
	storeCmd.AddCommand(adoptCmd)
}
