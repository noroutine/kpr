package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// runStatus renders banner + counters as text, for scripts and ssh.
// Redis down fails fast (every number would be a lie); a down registry
// only reddens the banner.
func runStatus(ctx context.Context, w io.Writer, s store.Store, reg *registry.Client, armed bool) error {
	if err := s.Ping(ctx); err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	rows, err := s.All(ctx)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	due := 0
	for _, r := range rows {
		if r.Due {
			due++
		}
	}
	acts, err := s.Activity(ctx)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	var performed, planned, failed int
	for _, a := range acts {
		switch a.Outcome {
		case "deleted":
			performed++
		case "planned":
			planned++
		case "failed":
			failed++
		}
	}
	registryState := "unreachable"
	if reg != nil {
		if rerr := reg.Reachable(ctx); rerr == nil {
			registryState = "reachable"
		}
	}
	arming := "dry-run"
	if armed {
		arming = "armed"
	}
	cur, _ := s.GetCurrent(ctx)
	_, err = fmt.Fprintf(w, "registry: %s\nredis: reachable\nsweeper: %s\ntracked: %d\ndue: %d\nperformed: %d\nplanned: %d\nfailed: %d\npass: %s (%s)\n",
		registryState, arming, len(rows), due, performed, planned, failed, cur.Stage, cur.Trigger)
	return err
}

// runPlan prints pending candidates with reasons; asJSON renders them
// for piping instead.
func runPlan(ctx context.Context, w io.Writer, s store.Store, asJSON bool) error {
	due, err := s.Due(ctx)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].Repo != due[j].Repo {
			return due[i].Repo < due[j].Repo
		}
		return due[i].Tag < due[j].Tag
	})
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

// EvaluatePolicies runs every policy over tracked rows plus live
// catalogs and returns the joined mark per row. Catalog failures skip
// that repo's catalog-dependent selectors (rows-only selectors still
// apply). Exported so the e2e scenarios (test/e2e) drive the same
// evaluation the CLI marks from — one policy path, never a copy.
func EvaluatePolicies(ctx context.Context, s store.Store, reg *registry.Client, now time.Time) ([]policy.Row, error) {
	rows, err := s.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis unreachable: %w", err)
	}
	marks := map[string][]string{}
	add := func(rs []policy.Row) {
		for _, r := range rs {
			k := r.Repo + "\x00" + r.Tag
			marks[k] = append(marks[k], r.Reason)
		}
	}
	add(policy.SelectExpired(rows, now))
	add(policy.SelectStaleUploads(rows, now))

	repos := map[string]bool{}
	for _, r := range rows {
		repos[r.Repo] = true
	}
	catalogs := map[string][]string{}
	if reg != nil {
		for repo := range repos {
			tags, cerr := reg.Catalog(ctx, repo)
			if cerr != nil {
				continue
			}
			catalogs[repo] = tags
		}
	}
	add(policy.SelectUntagged(rows, catalogs, now))
	add(policy.SelectKeepN(rows, policy.KeepN, nil, nil, now))

	var out []policy.Row
	for _, r := range rows {
		if reasons, ok := marks[r.Repo+"\x00"+r.Tag]; ok {
			r.Due = true
			r.Reason = strings.Join(reasons, "; ")
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Tag < out[j].Tag
	})
	return out, nil
}

// runReap evaluates the policies and, when armed, marks rows due.
// Unarmed it only prints the plan (same source as plan will show once
// marked): dry-run is implicit, --no-dry-run explicit.
func runReap(ctx context.Context, w io.Writer, s store.Store, reg *registry.Client, armed bool, now time.Time) error {
	marked, err := EvaluatePolicies(ctx, s, reg, now)
	if err != nil {
		return err
	}
	if !armed {
		if len(marked) == 0 {
			_, err := io.WriteString(w, "nothing due (dry-run)\n")
			return err
		}
		for _, r := range marked {
			if _, err := fmt.Fprintf(w, "%s:%s — %s\n", r.Repo, r.Tag, r.Reason); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "(dry-run: nothing marked; re-run with --no-dry-run to mark)\n")
		return err
	}
	for _, r := range marked {
		if merr := s.MarkDue(ctx, r.Repo, r.Tag, r.Reason); merr != nil {
			return fmt.Errorf("redis unreachable: %w", merr)
		}
	}
	_, err = fmt.Fprintf(w, "marked %d rows due\n", len(marked))
	return err
}

// runSweep POSTs the console trigger and prints the pass summary. When
// the console cannot be reached it degrades to the tick backstop with
// the due count.
func runSweep(ctx context.Context, w io.Writer, s store.Store, consoleURL string) error {
	url := strings.TrimSuffix(consoleURL, "/") + "/api/sweep"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return sweepUnreached(ctx, w, s)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return sweepUnreached(ctx, w, s)
	}
	var sum sweep.Summary
	if derr := json.NewDecoder(resp.Body).Decode(&sum); derr != nil {
		return sweepUnreached(ctx, w, s)
	}
	_, err = fmt.Fprintf(w, "sweep %s: %d performed, %d planned, %d failed, %d untracked\n",
		sum.PassID, sum.Performed, sum.Planned, sum.Failed, sum.Untracked)
	if err != nil {
		return err
	}
	for _, f := range sum.Failures {
		if _, err := fmt.Fprintf(w, "  failed: %s\n", f); err != nil {
			return err
		}
	}
	return nil
}

func sweepUnreached(ctx context.Context, w io.Writer, s store.Store) error {
	due, err := s.Due(ctx)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	_, err = fmt.Fprintf(w, "%d rows due, sweeper not reached — next tick picks them up\n", len(due))
	return err
}

// OpenStore dials the configured redis, failing fast with a clear
// error: every keeper command needs state, and inventing numbers
// without it is worse than refusing. Exported so the e2e suite
// (test/e2e) opens state the same way every command does — auth, DB
// selection, and refusal included.
func OpenStore(cfg *config.Config) (*store.RedisStore, error) {
	s := store.NewRedisStore(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("redis unreachable at %s: %w", cfg.RedisAddr, err)
	}
	return s, nil
}

// consoleURL builds the management console base from the resolved
// config (the sweep trigger lives on the server that already exists).
func consoleURL(cfg *config.Config) string {
	return "http://" + net.JoinHostPort(cfg.ManagementHost, strconv.Itoa(cfg.ManagementPort))
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show keeper banner and counters as text",
	Long:  `Banner plus counters from tracked state, for scripts and ssh. Needs redis; fails fast without it.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		s, err := OpenStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		return runStatus(cmd.Context(), cmd.OutOrStdout(), s, registry.NewClient(cfg.RegistryURL), cfg.NoDryRun)
	},
}

var planJSON bool

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show pending sweep candidates with reasons",
	Long:  `Pending candidates (rows marked due) with reasons. --json renders them for piping.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		s, err := OpenStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		return runPlan(cmd.Context(), cmd.OutOrStdout(), s, planJSON)
	},
}

var reapNoDryRun bool

var reapCmd = &cobra.Command{
	Use:   "reap",
	Short: "Evaluate policies and mark rows due",
	Long: `Evaluate the programmatic policies and mark selected rows due
with reasons. Dry-run unless --no-dry-run (or KPR_NO_DRY_RUN=true):
unarmed, it only prints the plan.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		s, err := OpenStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		armed := reapNoDryRun || cfg.NoDryRun
		return runReap(cmd.Context(), cmd.OutOrStdout(), s,
			registry.NewClient(cfg.RegistryURL), armed, time.Now().UTC())
	},
}

var sweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Trigger a sweep pass and watch it to the summary",
	Long: `POST the console sweep trigger and print the pass summary.
No opinions, no marks: only rows already marked due are processed.
An unreachable console degrades to the tick backstop.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		s, err := OpenStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		return runSweep(cmd.Context(), cmd.OutOrStdout(), s, consoleURL(cfg))
	},
}

func init() {
	planCmd.Flags().BoolVar(&planJSON, "json", false, "Render candidates as JSON for piping")
	reapCmd.Flags().BoolVar(&reapNoDryRun, "no-dry-run", false, "Mark rows due for real (default prints the plan only)")
	RootCmd.AddCommand(statusCmd, planCmd, reapCmd, sweepCmd)
}
