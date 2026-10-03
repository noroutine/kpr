package cli

import (
	"fmt"
	"io"
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
Preview by default; --no-dry-run records. Two lines repaint
live on a terminal (what the catalog names, what the store
holds against it); mid-run warnings break above them onto
their own lines. The per-tag stream goes to --output (- for
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
			// The stream is the feedback: the block stays
			// dark and the settled lines follow it.
			opts.Log = out
		} else {
			opts.Progress = func(sum backfill.Summary) {
				live.tickBlock(backfillLines(sum))
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
		// Preview announces itself up front — small view, said
		// before the run spends API calls, never as a trailing
		// suffix on the settled lines.
		if opts.DryRun {
			if _, err := fmt.Fprintln(out, "dry run — preview only, nothing recorded"); err != nil {
				return err
			}
		}
		// Warnings share the terminal with the repaint: each
		// breaks the block onto its own line first.
		sum, err := backfill.Run(cmd.Context(), breakWriter{w: out, live: live}, d.reg, d.reg,
			d.store, d.store, d.store, d.store, root,
			opts,
			proof.Force(armed, acceptRollback))
		live.doneBlock(backfillLines(sum), 0)
		return err
	},
}

// backfillLines renders the two-line block: what the catalog
// names, and what the store holds against it — the tracked
// baseline plus running verdicts. Values share analyze's
// column, so the blocks scan as one.
func backfillLines(sum backfill.Summary) []string {
	noun := "sentinels"
	if sum.Sentinels == 1 {
		noun = "sentinel"
	}
	return []string{
		analyzeRow("catalog", fmt.Sprintf("%d repos, %d tags", sum.Repos, sum.Tags)),
		analyzeRow("store", fmt.Sprintf("%d tracked (+%d %s), %d recorded, %d skipped, %d failed",
			sum.Tracked, sum.Sentinels, noun, sum.Recorded, sum.Skipped, sum.Failed)),
	}
}

// breakWriter ends an in-progress live repaint before an inline
// note (mid-run warnings share the terminal with the counters):
// the note lands on its own line and the next tick repaints the
// block fresh below it. Off-terminal it is a passthrough.
type breakWriter struct {
	w    io.Writer
	live *liveLines
}

func (b breakWriter) Write(p []byte) (int, error) {
	b.live.breakLine()
	return b.w.Write(p)
}

func init() {
	storeBackfillCmd.Flags().Bool("no-dry-run", false, "Record absent rows for real (default previews)")
	storeBackfillCmd.Flags().Bool("accept-rollback", false, "Record against a restored older generation (a rollback may have resurrected blobs)")
	storeBackfillCmd.Flags().String("output", "", "Per-tag stream sink: - for stdout, a path for a file (default discards, counters stay)")
	storeCmd.AddCommand(storeBackfillCmd)
}
