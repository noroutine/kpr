package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

var (
	backfillNoDryRun       bool
	backfillAcceptRollback bool
	backfillOutput         string
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
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		armed := proof.Arm(backfillNoDryRun, d.Cfg.CLINoDryRun)
		glob := ""
		if len(args) == 1 {
			glob = args[0]
		}
		out := cmd.OutOrStdout()
		live := newLiveLines(out)
		opts := backfill.Options{
			RepoGlob: glob,
			Armed:    armed,
		}
		stream, tick, closeSink, err := resolveBackfillSink(backfillOutput, out, live)
		if err != nil {
			return err
		}
		defer closeSink()
		// Preview announces itself up front — small view, said
		// before the run spends API calls, never as a trailing
		// suffix on the settled lines.
		if proof.Unarmed(armed) {
			if _, err := fmt.Fprintln(out, "dry run — preview only, nothing recorded"); err != nil {
				return err
			}
		}
		// Warnings share the terminal with the repaint: each
		// breaks the block onto its own line first.
		sum, err := backfill.Run(cmd.Context(), breakWriter{w: out, live: live}, backfill.Deps{
			API: d.Reg, Reg: d.Reg, Rows: d.Store,
			Rec: d.Store, Ids: d.Store, Lock: d.Store,
			Log: stream, Progress: tick,
		},
			opts,
			backfill.Accepts{Rollback: proof.Force(armed, backfillAcceptRollback)})
		live.doneBlock(backfillLines(sum), 0)
		return err
	},
}

// resolveBackfillSink maps --output to the per-tag stream and the
// live repaint: "-" streams on stdout (the stream is the feedback,
// the block stays dark), a path streams into the created file,
// empty discards the stream (nil log, the live repaint drives the
// terminal). The file must exist before the run spends API calls,
// so a bad path refuses here. Returns the stream, the repaint,
// and the stream's closer, if any.
func resolveBackfillSink(output string, out io.Writer, live *liveLines) (io.Writer, func(backfill.Summary), func(), error) {
	var stream io.Writer
	var tick func(backfill.Summary)
	if output == "-" {
		stream = out
	} else {
		tick = func(sum backfill.Summary) {
			live.tickBlock(backfillLines(sum))
		}
	}
	if output != "" && output != "-" {
		f, ferr := os.Create(output)
		if ferr != nil {
			return nil, nil, nil, ferr
		}
		return f, tick, func() { _ = f.Close() }, nil
	}
	return stream, tick, func() {}, nil
}

// backfillLines renders the three-line block: what the catalog
// names, what the store holds, and the run's own verdicts. The
// store line matches analyze's store line (tracked over
// everything, sentinels as a memo); only the backfill line moves
// per verdict. Labels pad one wider than analyze's — backfill is
// the longest noun here.
func backfillLines(sum backfill.Summary) []string {
	row := func(name, body string) string {
		return fmt.Sprintf("%-8s: %s", name, body)
	}
	noun := "sentinels"
	if sum.Sentinels == 1 {
		noun = "sentinel"
	}
	return []string{
		row("catalog", fmt.Sprintf("%s, %s",
			plural(sum.Repos, "repo", "repos"), plural(sum.Tags, "tag", "tags"))),
		row("store", fmt.Sprintf("%d tracked, %d %s",
			sum.Tracked+sum.Sentinels, sum.Sentinels, noun)),
		row("backfill", fmt.Sprintf("%d recorded, %d skipped, %d husks, %d failed",
			sum.Recorded, sum.Skipped, sum.Husks, sum.Failed)),
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
	storeBackfillCmd.Flags().BoolVar(&backfillNoDryRun, "no-dry-run", false, "Record absent rows for real (default previews)")
	storeBackfillCmd.Flags().BoolVar(&backfillAcceptRollback, "accept-rollback", false, "Record against a restored older generation (a rollback may have resurrected blobs)")
	storeBackfillCmd.Flags().StringVar(&backfillOutput, "output", "", "Per-tag stream sink: - for stdout, a path for a file (default discards, counters stay)")
	storeCmd.AddCommand(storeBackfillCmd)
}
