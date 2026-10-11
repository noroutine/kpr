package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// renderGatewayCard prints the fence posture from the shared
// model: open/deny/held with the endpoints beside it. Closed
// (no edge) renders as a state, never an error — same rule as
// the console section.
func renderGatewayCard(w io.Writer, card keeper.GatewayCard, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	edge := "closed"
	if card.Open {
		edge = "open"
	}
	fenceState := "pass"
	if card.Deny {
		fenceState = "deny"
	}
	hold := "no hold lease"
	if card.Held {
		hold = "hold lease pinning writes"
	}
	_, err := fmt.Fprintf(w, "edge: %s\nfence: %s (%s)\n", edge, fenceState, hold)
	if err != nil {
		return err
	}
	if card.Open && card.EdgeAddr != "" && card.RegistryURL != "" {
		_, err = fmt.Fprintf(w, "route: %s → %s\n", card.EdgeAddr, card.RegistryURL)
		return err
	}
	return nil
}

var statusGatewayCmd = &cobra.Command{
	Use:   "gateway",
	Short: "Show the live fence posture from the shared status model",
	Long: `Edge open/closed, fence deny/pass, hold lease — the
gateway card both the console and the CLI render.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		cfg := config.Current()
		var gate keeper.GatePosture
		if cfg.EdgeEnabled {
			// A read gate over the store's own lease medium:
			// the lock override in the fetch voices deny,
			// the lease read voices held. Fresh flips are
			// traffic's news, not a one-shot's.
			gate = &fence.Gate{Store: d.Store, Lease: fence.LeaseForStore(d.Store)}
		}
		card := keeper.FetchGatewayCard(cmd.Context(), gate, d.Store, cfg.EdgeAddr, cfg.RegistryURL)
		return renderGatewayCard(cmd.OutOrStdout(), card, asJSON)
	},
}

func init() {
	statusGatewayCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusGatewayCmd)
}
