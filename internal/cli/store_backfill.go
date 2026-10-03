package cli

import (
	"github.com/spf13/cobra"

	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

var storeBackfillCmd = &cobra.Command{
	Use:   "backfill [repo-glob]",
	Short: "Adopt pre-kpr tags into tracked rows",
	Long: `One-shot import for tags the receiver never saw: enumerates
the catalog (repo-glob scopes it, empty means all), HEADs every
tag's digest, and records the absent ones with their link mtimes
— signed kpr-backfill, never due. Tracked rows are no-ops;
mid-run vanishes skip by count. Gates per run on the served
generation without minting: stranger stores refuse, a restored
generation refuses armed unless --accept-rollback (a preview
warns through). A locked store refuses naming the ceremony.
Preview by default; --no-dry-run records.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		cfgPath, _ := cmd.Flags().GetString("config")
		noDryRun, _ := cmd.Flags().GetBool("no-dry-run")
		acceptRollback, _ := cmd.Flags().GetBool("accept-rollback")
		root, err := gc.StoreRoot(cfgPath)
		if err != nil {
			return err
		}
		armed := proof.Arm(noDryRun, d.cfg.CLINoDryRun)
		glob := ""
		if len(args) == 1 {
			glob = args[0]
		}
		_, err = backfill.Run(cmd.Context(), cmd.OutOrStdout(), d.reg, d.reg,
			d.store, d.store, d.store, d.store, root,
			backfill.Options{RepoGlob: glob, DryRun: gcDryRun(armed)},
			proof.Force(armed, acceptRollback))
		return err
	},
}

func init() {
	storeBackfillCmd.Flags().String("config", "/etc/distribution/config.yml", "Registry config file (shared store paths come from it)")
	storeBackfillCmd.Flags().Bool("no-dry-run", false, "Record absent rows for real (default previews)")
	storeBackfillCmd.Flags().Bool("accept-rollback", false, "Record against a restored older generation (a rollback may have resurrected blobs)")
	storeCmd.AddCommand(storeBackfillCmd)
}
