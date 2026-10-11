package cli

import (
	"encoding/json"
	"fmt"
	"io"
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
	n := min(len(card.Outcomes), keeper.ActivityTail)
	if _, err := fmt.Fprintf(w, "activity (last %d of %d):\n", n, card.Total); err != nil {
		return err
	}
	for _, o := range card.Outcomes[:n] {
		if _, err := fmt.Fprintln(w, keeper.FormatOutcome(now, o, true)); err != nil {
			return err
		}
	}
	return nil
}

// renderPlanCard prints due rows with reasons from the shared
// model: the read side of plan add/rm.
func renderPlanCard(w io.Writer, card keeper.PlanCard, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	// A backend that never answered reads unknown, never an
	// empty plan — silence here would misreport "nothing to do".
	if card.Standing != keeper.StandingOK {
		_, err := io.WriteString(w, "plan: unknown\n")
		return err
	}
	if len(card.Rows) == 0 {
		_, err := io.WriteString(w, "plan: empty\n")
		return err
	}
	for _, r := range card.Rows {
		pushed := r.PushedAt.UTC().Format(time.RFC3339)
		if r.PushedAt.IsZero() {
			pushed = "unknown"
		}
		if _, err := fmt.Fprintf(w, "%s:%s — %s (%s)\n", r.Repo, r.Tag, r.Reason, pushed); err != nil {
			return err
		}
	}
	return nil
}

var statusActivityCmd = &cobra.Command{
	Use:   "activity",
	Short: "Show the activity tail from the shared status model",
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

var statusPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show due rows from the shared status model",
	Long: `Due rows with reasons — the read side of plan add/rm,
the plan card both the console and the CLI render.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		card := keeper.FetchPlanCard(cmd.Context(), d.Store, nil)
		return renderPlanCard(cmd.OutOrStdout(), card, asJSON)
	},
}

func init() {
	statusActivityCmd.Flags().Bool("json", false, "Render the full ring as JSON")
	statusCmd.AddCommand(statusActivityCmd)
	statusPlanCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusPlanCmd)
}
