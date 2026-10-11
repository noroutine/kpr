package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// renderRegistryCard prints the probed registry from the shared
// model: reachability with the endpoint it probed. A down
// registry still names its URL; no prober reads unavailable in
// words — the standing code stays in --json, never in text.
func renderRegistryCard(w io.Writer, card keeper.RegistryCard, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	state := "reachable"
	if !card.Reachable {
		state = "unreachable — not answering"
		if card.Standing == keeper.StandingUnavailable {
			state = "unavailable — no registry wired"
		}
	}
	if _, err := fmt.Fprintf(w, "registry: %s\n", state); err != nil {
		return err
	}
	if card.URL != "" {
		_, err := fmt.Fprintf(w, "url: %s\n", card.URL)
		return err
	}
	return nil
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
