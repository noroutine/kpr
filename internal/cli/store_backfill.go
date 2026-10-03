package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"nrtn.dev/catalyst/kpr/internal/backfill"
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
Preview by default; --no-dry-run records. Counters repaint live
on a terminal; the per-tag stream goes to --output (- for
stdout, a path for a file) and is otherwise discarded.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		noDryRun, _ := cmd.Flags().GetBool("no-dry-run")
		acceptRollback, _ := cmd.Flags().GetBool("accept-rollback")
		output, _ := cmd.Flags().GetString("output")
		fsStore, err := proof.ProveFilesystemStore(d.cfg.RegistryConfig)
		if err != nil {
			return err
		}
		root := fsStore.Root()
		armed := proof.Arm(noDryRun, d.cfg.CLINoDryRun)
		glob := ""
		if len(args) == 1 {
			glob = args[0]
		}
		out := cmd.OutOrStdout()
		live := newLiveLines(out)
		opts := backfill.Options{
			RepoGlob: glob,
			DryRun:   gcDryRun(armed),
		}
		if output == "-" {
			// The stream is the feedback: counters would only
			// glue onto its lines.
			opts.Log = out
		} else {
			opts.Progress = func(sum backfill.Summary) {
				live.tick(fmt.Sprintf("backfill: %d recorded, %d skipped, %d failed",
					sum.Recorded, sum.Skipped, sum.Failed))
			}
		}
		if output != "" && output != "-" {
			f, ferr := os.Create(output)
			if ferr != nil {
				return ferr
			}
			defer func() { _ = f.Close() }()
			opts.Log = f
		}
		sum, err := backfill.Run(cmd.Context(), out, d.reg, d.reg,
			d.store, d.store, d.store, d.store, root,
			opts,
			proof.Force(armed, acceptRollback))
		live.done(fmt.Sprintf("backfill: %d recorded, %d skipped, %d failed",
			sum.Recorded, sum.Skipped, sum.Failed))
		return err
	},
}

func init() {
	storeBackfillCmd.Flags().Bool("no-dry-run", false, "Record absent rows for real (default previews)")
	storeBackfillCmd.Flags().Bool("accept-rollback", false, "Record against a restored older generation (a rollback may have resurrected blobs)")
	storeBackfillCmd.Flags().String("output", "", "Per-tag stream sink: - for stdout, a path for a file (default discards, counters stay)")
	storeCmd.AddCommand(storeBackfillCmd)
}
