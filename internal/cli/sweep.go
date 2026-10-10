package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// sweepPeer is the registry as the sweeper consumes it: deletes
// plus the sentinel read port. *registry.Client is the production
// adapter; tests bring stubs, never a loopback server.
type sweepPeer interface {
	sweep.Registry
	sentinel.API
}

// sweepLines renders the one-line block: the pass id plus its
// verdicts, repainting in place and converging to the settled
// summary below.
func sweepLines(sum sweep.Summary) []string {
	return []string{fmt.Sprintf("sweep %s: %d performed, %d planned, %d failed, %d untracked",
		sum.PassID, sum.Performed, sum.Planned, sum.Failed, sum.Untracked)}
}

// runSweep runs one sweep pass in-process with a live line on
// terminals: counters repaint in place and converge to the
// settled summary. Failure lines stream to stdout always (an
// outage narrates, never counts quietly); --output implies
// per-row — the file carries every verdict (would sweep/swept
// with the digest, skips with the reason, failures) plus the
// settled summary, while stdout keeps the counters. Row records
// ride OTLP-only (the terminal belongs to the live line); the
// ring already holds every verdict. Evaluation and marking live
// in sweep.RunPass; this stays wiring and printing.
func runSweep(ctx context.Context, w io.Writer, s store.Store, peer sweepPeer, armed proof.ArmedRun, output string) error {
	live := newLiveLines(w)
	failures := io.Writer(breakWriter{w: w, live: live})
	var logFile *os.File
	if output != "" && output != "-" {
		f, ferr := os.Create(output)
		if ferr != nil {
			return ferr
		}
		defer func() { _ = f.Close() }()
		logFile = f
		failures = io.MultiWriter(failures, f)
	}
	sw := &sweep.Sweeper{Store: s, Registry: peer, Sentinel: peer, Armed: armed}
	if logFile != nil {
		// --output implies per-row: the file carries every
		// verdict (would sweep/swept, skips, failures), stdout
		// keeps the counters.
		sw.RowLog = logFile
	}
	sw.Progress = func(sum sweep.Summary) {
		live.tickBlock(sweepLines(sum))
	}
	sum := sw.RunPass(ctx, "sweep")
	lines := sweepLines(sum)
	if proof.Unarmed(armed) {
		lines[0] += " (dry run — nothing deleted)"
	}
	if live.terminal() {
		live.doneBlock(lines, 0)
	} else if _, err := fmt.Fprintln(w, lines[0]); err != nil {
		// A half-printed summary must not read as success:
		// the repaint path is best-effort, the pipe is not.
		return err
	}
	if logFile != nil {
		for _, l := range lines {
			if _, err := fmt.Fprintln(logFile, l); err != nil {
				return err
			}
		}
	}
	for _, f := range sum.Failures {
		if _, err := fmt.Fprintf(failures, "  failed: %s\n", f); err != nil {
			return err
		}
	}
	return nil
}

var (
	sweepNoDryRun bool
	sweepOutput   string
)

var sweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Run one sweep pass in-process and print its summary",
	Long: `One sweep pass, in-process. Only rows already due are processed.
Row records ride OTLP-only; failures stream on stdout, --output
files the per-row log.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		cfg, s := d.Cfg, d.Store
		output, _ := cmd.Flags().GetString("output")
		return runSweep(cmd.Context(), cmd.OutOrStdout(), s, d.Reg, proof.Arm(sweepNoDryRun, cfg.CLINoDryRun), output)
	},
}

func init() {
	sweepCmd.Flags().BoolVar(&sweepNoDryRun, "no-dry-run", false, "Delete due rows for real (default plans only)")
	sweepCmd.Flags().StringVar(&sweepOutput, "output", "", "Write the per-row log plus summary into a file (stdout keeps counters and failures)")
	RootCmd.AddCommand(sweepCmd)
}
