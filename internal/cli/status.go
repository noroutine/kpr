package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/helpers/human"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

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
		return fmt.Errorf("%s unreachable: no tracked state to report", deps.StoreName(s))
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
	return p.Gen + " (" + human.Ago(age) + ")"
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
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show keeper banner and counters as text",
	Long:  `Banner plus counters from tracked state, for scripts and ssh.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		return runStatus(cmd.Context(), cmd.OutOrStdout(), d.Store, d.Reg)
	},
}

func init() {
	RootCmd.AddCommand(statusCmd)
}
