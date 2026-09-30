package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/gc"
)

var unlockConfigPath string

var lockCmd = &cobra.Command{
	Use:   "lock",
	Short: "Deny kpr registry-store writes",
	Long: `Drop the unlock marker: gc and every future store writer refuse
until 'kpr unlock' proves the shared store again. Reads, sweeps,
and the receiver keep working — only writes under the registry's
store go away. Doubles as the remote-mode simulator: locked behaves
exactly like no shared store for write ops.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		if err := d.store.SetUnlocked(cmd.Context(), false); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "store locked: registry-store writes denied until 'kpr unlock'")
		return err
	},
}

var unlockCmd = &cobra.Command{
	Use:   "unlock",
	Short: "Prove the shared store and allow kpr writes",
	Long: `Mint a fresh sentinel generation onto the shared store, read
it back through the API, and record the intent to allow
registry-store writes (gc and future writers). Refuses on a
stranger's store or no store at all — the marker never opens
without proof. Fresh stores start locked: unlock once per deploy,
lock to revoke.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		return gc.Unlock(cmd.Context(), cmd.OutOrStdout(), d.reg, unlockConfigPath, d.store)
	},
}

func init() {
	unlockCmd.Flags().StringVar(&unlockConfigPath, "config", "/etc/distribution/config.yml", "Registry config file (shared store paths come from it)")
	RootCmd.AddCommand(lockCmd, unlockCmd)
}
