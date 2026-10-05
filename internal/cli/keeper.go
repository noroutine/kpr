package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/clideps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// runStatus renders banner + counters as text, for scripts and ssh.
// The numbers come from keeper.FetchStatus — the same implementation
// the console reads. Redis down fails fast (every number would be a
// lie); a down registry only reddens the banner.
// statusRegistry is what the banner reads: reachability plus the
// live generation. *registry.Client carries both; tests pass it or
// nil (nil reddens the registry and unproves the proof, never
// panics).
type statusRegistry interface {
	keeper.Prober
	sentinel.API
}

// runStatus prints the ssh-and-scripts banner: reachability plus
// counters from tracked state. It reports no arming — arming is a
// per-invocation property of one-shot commands (their flags), not a
// persistent posture, and the serve loop that once owned it is gone.
func runStatus(ctx context.Context, w io.Writer, s store.Store, reg statusRegistry) error {
	st := keeper.FetchStatus(ctx, s, reg)
	if !st.StoreOK {
		return fmt.Errorf("%s unreachable: no tracked state to report", clideps.StoreName(s))
	}
	registryState := "unreachable"
	if st.RegistryOK {
		registryState = "reachable"
	}
	_, err := fmt.Fprintf(w, "registry: %s\ntracked: %d\ndue: %d\nperformed: %d\nplanned: %d\nfailed: %d\npass: %s (%s)\n",
		registryState, st.Tracked, st.Due, st.Performed, st.Planned, st.Failed, st.Current.Stage, st.Current.Trigger)
	return err
}

// lockState voices the intent marker in one word: locked denies
// registry-store writes, unlocked allows them, unknown means the
// read itself failed (never collapsed into either).
func lockState(ctx context.Context, s store.Store) string {
	ok, err := s.IsUnlocked(ctx)
	if err != nil {
		return "unknown"
	}
	if ok {
		return "unlocked"
	}
	return "locked"
}

// proofState voices the live generation with its age, or unproven
// when the registry serves none. Nil registry reads unproven;
// an unreadable timestamp still names the generation.
func proofState(ctx context.Context, api sentinel.API) string {
	if api == nil {
		return "unproven"
	}
	p, err := sentinel.LastProof(ctx, api)
	if err != nil {
		return "unproven"
	}
	age, err := p.Age(time.Now().UTC())
	if err != nil {
		return p.Gen
	}
	return p.Gen + " (" + age.Round(time.Second).String() + " ago)"
}

// describeStore names the wired backend with its address for the
// banner: file with its dir, redis with addr and DB. Mirrors the
// console's store card; both stay dumb views over the same facts.
func describeStore(s store.Store, cfg *config.Config) string {
	if st, ok := s.(*store.FileStore); ok {
		return "file (" + st.Dir() + ")"
	}
	return "redis (" + cfg.RedisAddr + " db " + strconv.Itoa(cfg.RedisDB) + ")"
}

// runPlan prints pending candidates with reasons; asJSON renders them
// for piping instead. The candidates come from keeper.ListPlan; this
// stays rendering-only.
func runPlan(ctx context.Context, w io.Writer, s store.Store, asJSON bool) error {
	due, err := keeper.ListPlan(ctx, s)
	if err != nil {
		return err
	}
	if asJSON {
		type candidate struct {
			Repo   string `json:"repo"`
			Tag    string `json:"tag"`
			Reason string `json:"reason"`
		}
		out := make([]candidate, 0, len(due))
		for _, r := range due {
			out = append(out, candidate{Repo: r.Repo, Tag: r.Tag, Reason: r.Reason})
		}
		return json.NewEncoder(w).Encode(out)
	}
	if len(due) == 0 {
		_, err := io.WriteString(w, "nothing due\n")
		return err
	}
	for _, r := range due {
		if _, err := fmt.Fprintf(w, "%s:%s — %s\n", r.Repo, r.Tag, r.Reason); err != nil {
			return err
		}
	}
	return nil
}

// runReap renders the reap verdict: the plan in both modes —
// rows read identically dry or armed, only the trailer says
// whether they were marked. Evaluation and marking live in
// keeper.Reap; this stays printing-only.
func runReap(ctx context.Context, w io.Writer, s store.Store, reg keeper.CatalogSource, armed bool, excludes []string, now time.Time, policyName string) error {
	marked, err := keeper.Reap(ctx, s, reg, now, excludes, policyName, armed)
	if err != nil {
		return err
	}
	if len(marked) == 0 {
		if armed {
			_, err := io.WriteString(w, "marked 0 rows due\n")
			return err
		}
		_, err := io.WriteString(w, "nothing due (dry-run)\n")
		return err
	}
	for _, r := range marked {
		if _, err := fmt.Fprintf(w, "%s:%s — %s\n", r.Repo, r.Tag, r.Reason); err != nil {
			return err
		}
	}
	if armed {
		_, err = fmt.Fprintf(w, "marked %d rows due\n", len(marked))
		return err
	}
	_, err = io.WriteString(w, "(dry-run: nothing marked; re-run with --no-dry-run to mark)\n")
	return err
}

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
func runSweep(ctx context.Context, w io.Writer, s store.Store, peer sweepPeer, armed bool, output string) error {
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
	sw := &sweep.Sweeper{Store: s, Registry: peer, Sentinel: peer, DryRun: !armed}
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
	if !armed {
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

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show keeper banner and counters as text",
	Long:  `Banner plus counters from tracked state, for scripts and ssh. Needs state; fails fast without it. Store facts (backend, lock, proof, identity) live under 'store status'.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := clideps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		return runStatus(cmd.Context(), cmd.OutOrStdout(), d.Store, d.Reg)
	},
}

var planJSON bool

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show pending sweep candidates with reasons",
	Long:  `Pending candidates (rows marked due) with reasons. --json renders them for piping.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := clideps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		s := d.Store
		return runPlan(cmd.Context(), cmd.OutOrStdout(), s, planJSON)
	},
}

// runDiscardPlan reports the discard count. No dry-run: discarding
// previews nothing — plan already showed the rows. The marks drop in
// keeper.DiscardPlan; this stays reporting-only.
func runDiscardPlan(ctx context.Context, w io.Writer, s store.Store) error {
	n, err := keeper.DiscardPlan(ctx, s)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err := io.WriteString(w, "nothing due\n")
		return err
	}
	_, err = fmt.Fprintf(w, "discarded %d due marks\n", n)
	return err
}

var reapNoDryRun bool

var reapExclude []string

var reapCmd = &cobra.Command{
	Use:   "reap [policy]",
	Short: "Evaluate policies and mark rows due",
	Long: `Evaluate one policy (or all) and mark selected rows due with
reasons. Bare reap means reap all. Marks accumulate across calls
until sweep or plan discard. Policies: ttl (elapsed explicit TTL),
hash (bare hashes past the default), partial (digest-less stale
uploads), untagged (tag gone from the catalog past grace), keep-n
(past the freshest ten per repo).
Dry-run unless --no-dry-run (or KPR_CLI_NO_DRY_RUN=true): unarmed, it
only prints the plan. Repeat --exclude to spare keep-N for rows
whose repo:tag matches (registry stripped).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := clideps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		cfg, s := d.Cfg, d.Store
		armed := reapArmed(cfg)
		name := "all"
		if len(args) == 1 {
			name = args[0]
		}
		return runReap(cmd.Context(), cmd.OutOrStdout(), s,
			d.Reg, armed, reapExclude, time.Now().UTC(), name)
	},
}

var planDiscardCmd = &cobra.Command{
	Use:   "discard",
	Short: "Drop the whole plan (clear all due marks)",
	Long: `Clear every due mark. Rows survive; only marks go, so the next
sweep finds nothing until a fresh reap marks again.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := clideps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		s := d.Store
		return runDiscardPlan(cmd.Context(), cmd.OutOrStdout(), s)
	},
}

var sweepNoDryRun bool
var sweepOutput string

var sweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Run one sweep pass in-process and print its summary",
	Long: `Run one sweep pass in-process and print the pass summary.
No opinions, no marks: only rows already marked due are processed.
The sweeper lives here, not in serve (serve serves endpoints; it
never sweeps). --no-dry-run (or KPR_CLI_NO_DRY_RUN=true) arms
it: deletes for real. Disarmed plans only. Counters repaint one
live line on a terminal and converge to the summary; row
records ride OTLP-only (stdout stays quiet), failure lines
stream on stdout. --output writes the per-row log (would
sweep/swept, skips, failures) plus the summary into a file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := clideps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		cfg, s := d.Cfg, d.Store
		output, _ := cmd.Flags().GetString("output")
		return runSweep(cmd.Context(), cmd.OutOrStdout(), s, d.Reg, sweepArmed(cfg), output)
	},
}

func init() {
	planCmd.Flags().BoolVar(&planJSON, "json", false, "Render candidates as JSON for piping")
	planCmd.AddCommand(planDiscardCmd, planAddCmd, planRemoveCmd)
	reapCmd.Flags().BoolVar(&reapNoDryRun, "no-dry-run", false, "Mark rows due for real (default prints the plan only)")
	reapCmd.Flags().StringSliceVar(&reapExclude, "exclude", nil, "Spare keep-N for rows whose repo:tag matches (repeatable regex, registry stripped)")
	sweepCmd.Flags().BoolVar(&sweepNoDryRun, "no-dry-run", false, "Delete due rows for real (default plans only)")
	sweepCmd.Flags().StringVar(&sweepOutput, "output", "", "Write the per-row log plus summary into a file (stdout keeps counters and failures)")
	RootCmd.AddCommand(statusCmd, planCmd, reapCmd, sweepCmd)
}

// reapArmed is the same wiring as sweepArmed: the flag arms one
// invocation, KPR_CLI_NO_DRY_RUN arms every one-shot. If this fails,
// `reap` answers to the wrong var — the blind spot the sweep review
// found, mirrored here before it bites.
func reapArmed(cfg *config.Config) bool {
	return reapNoDryRun || cfg.CLINoDryRun
}

// sweepArmed is the command's arming wiring, factored for test: the
// flag arms one invocation, KPR_CLI_NO_DRY_RUN arms every one-shot
// (gc, reap, sweep alike). If this fails, `sweep` answers to the
// wrong var — the split's leftover coupling, back again.
func sweepArmed(cfg *config.Config) bool {
	return sweepNoDryRun || cfg.CLINoDryRun
}
