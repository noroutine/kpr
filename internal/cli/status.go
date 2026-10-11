package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/helpers/human"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// statusRegistry is what the banner reads: reachability plus the
// live generation. *registry.Client carries both; tests pass it or
// nil (nil reddens the registry and unproves the proof, never
// panics).
type statusRegistry interface {
	keeper.Prober
	sentinel.API
}

// runStatus prints the high-level posture, one line per section —
// the same words the detail views voice, without their detail.
// A dead store fails fast: without state every number would be a
// lie; a down registry only reddens its line. It reports no
// arming — arming is a per-invocation property of one-shot
// commands (their flags), not a persistent posture, and the
// serve loop that once owned it is gone.
func runStatus(w io.Writer, reg keeper.RegistryCard, card keeper.StoreCard, st keeper.Status, edge keeper.EdgeCard, clock keeper.ClockCard, obs keeper.ObsCard) error {
	if !st.StoreOK {
		return fmt.Errorf("%s unreachable: no tracked state to report", card.Name)
	}
	url := ""
	if reg.URL != "" {
		url = " (" + reg.URL + ")"
	}
	if _, err := fmt.Fprintf(w, "registry: %s%s\n", registryState(reg), url); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "store: %s (%s)\n", storeState(card.Standing), card.Name); err != nil {
		return err
	}
	posture := "closed"
	if edge.Open {
		posture = "open, pass"
		if edge.Deny {
			posture = "open, deny"
		}
	}
	if _, err := fmt.Fprintf(w, "edge: %s\n", posture); err != nil {
		return err
	}
	// The skew voices itself when it is a reading; unreachable
	// and unchecked have no reading (their SkewText is the
	// standing or a dash), so the line names the state with the
	// transport behind it instead of doubling the word.
	skew := fmt.Sprintf("%s via %s", clock.SkewText(), clock.Method)
	switch clock.Standing {
	case keeper.StandingOK:
	case keeper.StandingUnreachable, keeper.StandingUnchecked:
		skew = fmt.Sprintf("%s (%s)", clock.Standing, clock.Method)
	default:
		skew = fmt.Sprintf("%s: %s", clock.Standing, skew)
	}
	if _, err := fmt.Fprintf(w, "clock: %s\n", skew); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "plan: %d due\n", len(st.Plan)); err != nil {
		return err
	}
	activity := "none recorded"
	if len(st.Activity) > 0 {
		activity = fmt.Sprintf("%d recorded, last %s", len(st.Activity), st.Activity[0].Outcome)
	}
	if _, err := fmt.Fprintf(w, "activity: %s\n", activity); err != nil {
		return err
	}
	backends := "unavailable"
	if obs.Standing == keeper.StandingOK {
		backends = fmt.Sprintf("%d backend", len(obs.Links))
		if len(obs.Links) != 1 {
			backends += "s"
		}
	}
	_, err := fmt.Fprintf(w, "observability: %s\n", backends)
	return err
}

// proofState voices the live generation with its age, or unproven
// when the registry serves none. Nil registry reads unproven;
// an unreadable timestamp still names the generation.
func proofState(ctx context.Context, api sentinel.API) string {
	card := keeper.FetchProofCard(ctx, api)
	if !card.Proven() {
		return "unproven"
	}
	age, err := card.Age(time.Now().UTC())
	if err != nil {
		return card.Gen
	}
	return card.Gen + " (" + human.Ago(age) + ")"
}

// runPlan prints pending candidates with reasons; asJSON renders them
// for piping instead. The candidates come from keeper.ListPlan; this
// stays rendering-only.
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show high-level posture of every section",
	Long: `One line per section from the shared status model — the
detail lives behind each status card.`,
	// Cards are subcommands; a stray arg is a typo, never the
	// posture — NoArgs turns it into unknown-command before RunE.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		cfg := config.Current()
		var gate keeper.GatePosture
		if cfg.EdgeEnabled {
			gate = &fence.Gate{Store: d.Store, Lease: fence.LeaseForStore(d.Store)}
		}
		// Reads ride a bounded context: a dead backend reads
		// degraded/unreachable, never slow (house 5s).
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()
		st := keeper.FetchStatus(ctx, d.Store, d.Reg)
		proven := keeper.FetchProofCard(ctx, d.Reg).Proven()
		return runStatus(cmd.OutOrStdout(),
			keeper.FetchRegistryCard(ctx, d.Reg, cfg.RegistryURL),
			keeper.FetchStoreCard(ctx, d.Store, st.StoreOK, cfg.RedisAddr, cfg.RedisDB, proven),
			st,
			keeper.FetchEdgeCard(ctx, gate, d.Store, cfg.EdgeAddr, cfg.RegistryURL),
			keeper.FetchClockCard(ctx, clock.NewSource(cfg.TimeMethod), cfg.TimeMethod, cfg.TimeServer, clock.Tolerance),
			keeper.FetchObsCard(keeper.ObsURLs{
				Quickwit: cfg.QuickwitURL, Jaeger: cfg.JaegerURL,
				Grafana: cfg.GrafanaURL, Prometheus: cfg.PrometheusURL,
			}),
		)
	},
}

func init() {
	RootCmd.AddCommand(statusCmd)
}
