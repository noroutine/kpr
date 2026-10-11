package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// renderActivityCard prints the tail from the shared model,
// capped and said out loud: last n of total, one shared
// formatter per line. --json is the full ring with precise
// stamps — the /api/activity shape, superset.
func renderActivityCard(w io.Writer, card keeper.ActivityCard, now time.Time, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	// A backend that never answered reads unknown, never an
	// empty ring — the reference draws the same line.
	if card.Standing != keeper.StandingOK {
		_, err := io.WriteString(w, "activity: unknown\n")
		return err
	}
	// An empty ring is an idle keeper, not a broken one: it
	// reads none recorded — the same words the aggregate
	// posture and the old reference voice.
	if len(card.Outcomes) == 0 {
		_, err := io.WriteString(w, "activity: none recorded\n")
		return err
	}
	n := min(len(card.Outcomes), keeper.ActivityTail)
	// One assembled write: the tail reads whole or errors — a
	// broken pipe never prints a truncated ring as success.
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "activity (last %d of %d):\n", n, card.Total)
	for _, o := range card.Outcomes[:n] {
		_, _ = fmt.Fprintln(&sb, keeper.FormatOutcome(now, o, true))
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

var statusActivityCmd = &cobra.Command{
	Use:   "activity",
	Short: "Show the activity tail",
	Long: `Last ring entries with the honest total — the activity
card both the console and the CLI render.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		card := keeper.FetchActivityCard(cmd.Context(), d.Store)
		return renderActivityCard(cmd.OutOrStdout(), card, time.Now().UTC(), asJSON)
	},
}

func init() {
	statusActivityCmd.Flags().Bool("json", false, "Render the full ring as JSON")
	statusCmd.AddCommand(statusActivityCmd)
}
