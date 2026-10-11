package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// renderObsCard prints the observability backends from the
// shared model: one line per link, name with URL and blurb. An
// unconfigured stack reads unavailable in words — the standing
// code stays in --json, never in the text. No probing:
// configured is the whole fact.
func renderObsCard(w io.Writer, card keeper.ObsCard, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	if card.Standing != keeper.StandingOK {
		_, err := io.WriteString(w, "observability: unavailable — no backends configured\n")
		return err
	}
	// One assembled write: every link or an error, no partial
	// launchpad printed as success.
	var sb strings.Builder
	for _, l := range card.Links {
		_, _ = fmt.Fprintf(&sb, "%s: %s — %s\n", l.Name, l.URL, l.Blurb)
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

var statusObservabilityCmd = &cobra.Command{
	Use:   "observability",
	Short: "Show the observability backends",
	Long: `Configured UI backends with their URLs — the same links
the console launchpad shows. Unconfigured reads unavailable.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		// Resolved and installed, not just read: the installed
		// global starts as the empty default, and this command
		// opens no store — without its own resolution it would
		// reason about nothing while serve links backends. Same
		// one value OpenDeps installs for the commands that do.
		cfg := config.NewBuilder().FromEnv().Build()
		config.SetCurrent(cfg)
		card := keeper.FetchObsCard(keeper.ObsURLs{
			Quickwit: cfg.QuickwitURL, Jaeger: cfg.JaegerURL,
			Grafana: cfg.GrafanaURL, Prometheus: cfg.PrometheusURL,
		})
		return renderObsCard(cmd.OutOrStdout(), card, asJSON)
	},
}

func init() {
	statusObservabilityCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusObservabilityCmd)
}
