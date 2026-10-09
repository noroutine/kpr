package cli

import (
	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/storeops"
)

var lockCmd = &cobra.Command{
	Use:   "lock",
	Short: "Deny kpr registry-store writes",
	Long: `Drop the unlock marker; gc and every future store writer refuse.
Reads, sweeps, and the receiver keep working.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		return storeops.Lock(cmd.Context(), cmd.OutOrStdout(), d.Store)
	},
}

func init() {
	storeCmd.AddCommand(lockCmd)
}
