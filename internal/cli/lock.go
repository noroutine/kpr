package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/gc"
)

var lockCmd = &cobra.Command{
	Use:   "lock",
	Short: "Deny kpr registry-store writes",
	Long: `Drop the unlock marker: gc and every future store writer refuse
until 'kpr store unlock' proves the shared store again. Reads, sweeps,
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
		// The marker denies; this says it out loud — the ring
		// carries deny_engage at the transition, not at the
		// first refused push.
		edge.Control{Store: d.store}.Deny(cmd.Context(), "store locked: registry-store writes denied until 'kpr store unlock'")
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "store locked: registry-store writes denied until 'kpr store unlock'")
		return err
	},
}

var unlockCmd = &cobra.Command{
	Use:   "unlock",
	Short: "Prove the shared store and allow kpr writes",
	Long: `Mint a fresh sentinel generation onto the shared store, read
it back through the API, and record the intent to allow
registry-store writes (gc and future writers). Reads first
through the same verdict gc uses: foreign, unpaired, stale, and
identity-less lineages refuse with the ceremony named (as does a
skewed clock — fix the clock and retry, there are no accept flags here).
Silence establishes the pairing. The marker never opens without
proof. Fresh stores start locked: unlock once per deploy, lock
to revoke.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		if err := gc.Unlock(cmd.Context(), cmd.OutOrStdout(), d.reg, d.cfg.RegistryConfig, d.store, d.store, d.store, d.store, clockSource(d.cfg), d.cfg.TimeServer); err != nil {
			return err
		}
		// Proof opened writes; this says it out loud — the ring
		// carries deny_release at the transition.
		edge.Control{Store: d.store}.Allow(cmd.Context(), "store unlocked: shared store proven, registry-store writes allowed")
		return nil
	},
}

func init() {
	storeCmd.AddCommand(lockCmd, unlockCmd)
}
