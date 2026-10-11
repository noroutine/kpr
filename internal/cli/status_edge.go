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
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// renderEdgeCard prints the fence posture from the shared
// model: open/deny/hold with the endpoints beside it. Closed
// (no edge) renders as a state, never an error — same rule as
// the console section.
func renderEdgeCard(w io.Writer, card keeper.EdgeCard, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	// One assembled write: a broken pipe errors with no partial
	// card printed as success. Closed renders edge state only:
	// the fence was never read, so a verdict would be
	// fabricated. The console guards its Fence card behind the
	// same open check.
	var sb strings.Builder
	if !card.Open {
		_, _ = fmt.Fprintf(&sb, "gateway: closed\n")
		_, err := io.WriteString(w, sb.String())
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
	if card.Hold {
		hold = "hold lease pinning writes"
	}
	_, _ = fmt.Fprintf(&sb, "gateway: open\nfence: %s (%s)\n", fenceState, hold)
	if card.Open && card.EdgeAddr != "" && card.RegistryURL != "" {
		_, _ = fmt.Fprintf(&sb, "route: %s → %s\n", card.EdgeAddr, card.RegistryURL)
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

var statusEdgeCmd = &cobra.Command{
	Use:   "edge",
	Short: "Show the live edge posture",
	Long: `Gateway open/closed, fence deny/pass, hold lease — the
edge card both the console and the CLI render. The edge is
the gateway in front of the registry.`,
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
			// the lease read voices hold. Fresh flips are
			// traffic's news, not a one-shot's.
			gate = &fence.Gate{Store: d.Store, Lease: fence.LeaseForStore(d.Store)}
		}
		// Lease and lock reads ride a bounded context: a dead
		// backend reads degraded/unreachable, never slow.
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()
		card := keeper.FetchEdgeCard(ctx, gate, d.Store, cfg.EdgeAddr, cfg.RegistryURL)
		return renderEdgeCard(cmd.OutOrStdout(), card, asJSON)
	},
}

func init() {
	statusEdgeCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusEdgeCmd)
}
