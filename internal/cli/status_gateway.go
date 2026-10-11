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
	// Closed renders edge state only: the fence was never read,
	// so a verdict would be fabricated. The console guards its
	// Fence card behind the same open check.
	if !card.Open {
		_, err := fmt.Fprintf(w, "edge: closed\n")
		return err
	}
	// The posture names its effect: deny refuses writes, pass
	// allows them. The bare names are the model's shorthand
	// (--json keeps them); the text view owes what they mean.
	fenceState := "pass — writes allowed"
	if card.Deny {
		fenceState = "deny — writes refused"
	}
	hold := "no hold lease"
	if card.Held {
		hold = "hold lease pinning writes"
	}
	_, err := fmt.Fprintf(w, "edge: open\nfence: %s (%s)\n", fenceState, hold)
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
		// Lease and lock reads ride a bounded context: a dead
		// backend reads degraded/unreachable, never slow.
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()
		card := keeper.FetchGatewayCard(ctx, gate, d.Store, cfg.EdgeAddr, cfg.RegistryURL)
		return renderGatewayCard(cmd.OutOrStdout(), card, asJSON)
	},
}

func init() {
	statusGatewayCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusGatewayCmd)
}
