package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// registryState translates the generic standing into
// registry-specific words: what the reachability means for the
// probed endpoint. Both the card view and the aggregate posture
// voice it; --json keeps the raw code for machines.
func registryState(card keeper.RegistryCard) string {
	if card.Reachable {
		return "reachable"
	}
	if card.Standing == keeper.StandingUnavailable {
		return "unavailable — no registry wired"
	}
	return "unreachable — not answering"
}

// renderRegistryCard prints the probed registry from the shared
// model: reachability with the endpoint it probed. A down
// registry still names its URL; no prober reads unavailable in
// words — the standing code stays in --json, never in text.
func renderRegistryCard(w io.Writer, card keeper.RegistryCard, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	// One assembled write, like the aggregate: a broken pipe
	// errors with no partial card printed as success.
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "registry: %s\n", registryState(card))
	if card.URL != "" {
		_, _ = fmt.Fprintf(&sb, "url: %s\n", card.URL)
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

var statusRegistryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Show the probed registry",
	Long: `Reachability with the probed endpoint — the registry
card both the console and the CLI render.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		cfg := config.Current()
		// The probe rides a bounded context: a dead registry
		// reads unreachable, never slow (house 5s).
		// NOTE(mutants): the 5s bound is timing, not logic — no
		// test distinguishes it from any other positive bound
		// without a stopwatch. A mutant here survives by being
		// unobservable, not by being correct.
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()
		card := keeper.FetchRegistryCard(ctx, d.Reg, cfg.RegistryURL)
		return renderRegistryCard(cmd.OutOrStdout(), card, asJSON)
	},
}

func init() {
	statusRegistryCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusRegistryCmd)
}
