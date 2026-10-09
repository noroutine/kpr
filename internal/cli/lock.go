package cli

import (
	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/fence"
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

var unlockCmd = &cobra.Command{
	Use:   "unlock",
	Short: "Prove the shared store and allow kpr writes",
	Long: `Mint a fresh sentinel generation, read it back through the API,
and allow registry-store writes. Foreign, unpaired, stale, and
identity-less lineages refuse with the ceremony named. Fresh
stores start locked: unlock once per deploy.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		if err := storeops.Unlock(cmd.Context(), cmd.OutOrStdout(), storeops.UnlockDeps{
			Rec: d.Store, Ids: d.Store, Rows: d.Store,
			API: d.Reg, Store: d.Store,
		}); err != nil {
			return err
		}
		// Proof opened writes; this says it out loud — the ring
		// carries deny_release at the transition.
		fence.Control{Store: d.Store}.Allow(cmd.Context(), "store unlocked: shared store proven, registry-store writes allowed")
		return nil
	},
}

func init() {
	storeCmd.AddCommand(lockCmd, unlockCmd)
}
