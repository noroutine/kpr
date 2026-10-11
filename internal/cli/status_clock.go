package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// renderClockCard prints the time transport from the shared
// model: method, server, live skew, bound, and the render stamp
// beside it — server time as kpr sees it, same shape as the
// console's Server time card. The stamp is caller-owned (Now
// stays out of the shared card); --json stays the card verbatim.
// Degraded reads loud (caps + the bound beside the skew); local
// reads unchecked, never a fake zero.
func renderClockCard(w io.Writer, card keeper.ClockCard, now time.Time, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	standing := ""
	if card.Standing != keeper.StandingOK {
		standing = " (" + string(card.Standing) + ")"
	}
	_, err := fmt.Fprintf(w, "method: %s\nserver: %s\nskew: %s%s\n%s\nnow: %s\n",
		card.Method, card.Server, card.SkewText(), standing, card.NoteText(),
		now.UTC().Format("2006-01-02 15:04:05 UTC"))
	return err
}

var statusClockCmd = &cobra.Command{
	Use:   "clock",
	Short: "Show the time transport from the shared status model",
	Long: `Method, server, live skew, bound — the clock card both
the console and the CLI render.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		cfg := config.Current()
		// The probe rides a bounded context: an unreachable
		// source reads unreachable, never slow (house 5s).
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()
		card := keeper.FetchClockCard(ctx,
			clock.NewSource(cfg.TimeMethod), cfg.TimeMethod, cfg.TimeServer, clock.Tolerance)
		return renderClockCard(cmd.OutOrStdout(), card, time.Now(), asJSON)
	},
}

func init() {
	statusClockCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusClockCmd)
}
