package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// runStatus renders banner + counters as text, for scripts and ssh.
// The numbers come from keeper.FetchStatus — the same implementation
// the console reads. Redis down fails fast (every number would be a
// lie); a down registry only reddens the banner.
func runStatus(ctx context.Context, w io.Writer, s store.Store, reg keeper.Prober, armed bool) error {
	st := keeper.FetchStatus(ctx, s, reg)
	if !st.StoreOK {
		return fmt.Errorf("%s unreachable: no tracked state to report", storeName(s))
	}
	registryState := "unreachable"
	if st.RegistryOK {
		registryState = "reachable"
	}
	arming := "dry-run"
	if armed {
		arming = "armed"
	}
	_, err := fmt.Fprintf(w, "registry: %s\nredis: reachable\nsweeper: %s\ntracked: %d\ndue: %d\nperformed: %d\nplanned: %d\nfailed: %d\npass: %s (%s)\n",
		registryState, arming, st.Tracked, st.Due, st.Performed, st.Planned, st.Failed, st.Current.Stage, st.Current.Trigger)
	return err
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

// runReap renders the reap verdict: the dry-run plan, or the marked
// count once armed. Evaluation and marking live in keeper.Reap; this
// stays printing-only.
func runReap(ctx context.Context, w io.Writer, s store.Store, reg keeper.CatalogSource, armed bool, excludes []string, now time.Time, policyName string) error {
	marked, err := keeper.Reap(ctx, s, reg, now, excludes, policyName, armed)
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

// resolveStoreBackend derives the state backend from explicit
// signals, never resolved values (KPR_REDIS_ADDR carries a default
// that must not count as a choice): KPR_STORE, when set, is
// authoritative and must agree with backend-specific variables;
// otherwise KPR_STORE_DIR alone selects file and KPR_REDIS_ADDR
// alone selects redis; silence keeps redis defaults. Empty counts as
// unset throughout. Mixed signals refuse instead of guessing.
func resolveStoreBackend() (backend, dir string, err error) {
	storeVar, storeSet := os.LookupEnv(config.EnvStore)
	dirVar, dirSet := os.LookupEnv(config.EnvStoreDir)
	redisVar, redisSet := os.LookupEnv(config.EnvRedisAddr)
	if storeVar == "" {
		storeSet = false
	}
	if dirVar == "" {
		dirSet = false
	}
	if redisVar == "" {
		redisSet = false
	}
	if storeSet {
		switch storeVar {
		case "file":
			if redisSet {
				return "", "", fmt.Errorf("KPR_STORE=file conflicts with %s: unset one", config.EnvRedisAddr)
			}
			if !dirSet {
				dirVar = config.DefaultStoreDir
			}
			return "file", dirVar, nil
		case "redis":
			if dirSet {
				return "", "", fmt.Errorf("KPR_STORE=redis conflicts with %s: unset one", config.EnvStoreDir)
			}
			return "redis", "", nil
		default:
			return "", "", fmt.Errorf("unknown KPR_STORE=%q: want file or redis", storeVar)
		}
	}
	if dirSet {
		return "file", dirVar, nil
	}
	return "redis", "", nil
}

// storeName voices which backend failed: the refusal names what the
// operator must fix, in either mode.
func storeName(s store.Store) string {
	if _, ok := s.(*store.FileStore); ok {
		return "file store"
	}
	return "redis"
}

// OpenStore opens the derived backend, failing fast with a clear
// error: every keeper command needs state, and inventing numbers
// without it is worse than refusing. Exported so the e2e suite
// (test/e2e) opens state the same way every command does — auth, DB
// selection, and refusal included.
func OpenStore(cfg *config.Config) (store.StoreCloser, error) {
	backend, dir, err := resolveStoreBackend()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if backend == "file" {
		s := store.NewFileStore(dir)
		if err := s.Ping(ctx); err != nil {
			return nil, fmt.Errorf("file store at %s unreachable: %w", dir, err)
		}
		return s, nil
	}
	s := store.NewRedisStore(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
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
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		cfg, s := d.cfg, d.store
		return runStatus(cmd.Context(), cmd.OutOrStdout(), s, d.reg, cfg.NoDryRun)
	},
}

var planJSON bool

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show pending sweep candidates with reasons",
	Long:  `Pending candidates (rows marked due) with reasons. --json renders them for piping.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		s := d.store
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
until sweep or plan discard. Policies: expired (elapsed TTL tags),
partial (digest-less stale uploads), untagged (tag gone from the
catalog past grace), keep-n (past the freshest ten per repo).
Dry-run unless --no-dry-run (or KPR_NO_DRY_RUN=true): unarmed, it
only prints the plan. Repeat --exclude to spare keep-N for rows
whose repo:tag matches (registry stripped).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		cfg, s := d.cfg, d.store
		armed := reapNoDryRun || cfg.NoDryRun
		name := "all"
		if len(args) == 1 {
			name = args[0]
		}
		return runReap(cmd.Context(), cmd.OutOrStdout(), s,
			d.reg, armed, reapExclude, time.Now().UTC(), name)
	},
}

var planDiscardCmd = &cobra.Command{
	Use:   "discard",
	Short: "Drop the whole plan (clear all due marks)",
	Long: `Clear every due mark. Rows survive; only marks go, so the next
sweep finds nothing until a fresh reap marks again.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		s := d.store
		return runDiscardPlan(cmd.Context(), cmd.OutOrStdout(), s)
	},
}

var sweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Trigger a sweep pass and watch it to the summary",
	Long: `POST the console sweep trigger and print the pass summary.
No opinions, no marks: only rows already marked due are processed.
An unreachable console degrades to the tick backstop.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		cfg, s := d.cfg, d.store
		return runSweep(cmd.Context(), cmd.OutOrStdout(), s, consoleURL(cfg))
	},
}

func init() {
	planCmd.Flags().BoolVar(&planJSON, "json", false, "Render candidates as JSON for piping")
	planCmd.AddCommand(planDiscardCmd, planAddCmd, planRemoveCmd)
	reapCmd.Flags().BoolVar(&reapNoDryRun, "no-dry-run", false, "Mark rows due for real (default prints the plan only)")
	reapCmd.Flags().StringSliceVar(&reapExclude, "exclude", nil, "Spare keep-N for rows whose repo:tag matches (repeatable regex, registry stripped)")
	RootCmd.AddCommand(statusCmd, planCmd, reapCmd, sweepCmd)
}
