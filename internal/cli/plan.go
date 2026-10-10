package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// runPlan prints pending candidates with reasons; asJSON renders them
// for piping instead. The candidates come from keeper.ListPlan; this
// stays rendering-only.
func runPlan(ctx context.Context, w io.Writer, s store.Store, asJSON bool) error {
	due, err := keeper.ListPlan(ctx, s)
	if err != nil {
		return err
	}
	if asJSON {
		type candidate struct {
			Repo   string `json:"repo"`
			Tag    string `json:"tag"`
			Reason string `json:"reason"`
		}
		out := make([]candidate, 0, len(due))
		for _, r := range due {
			out = append(out, candidate{Repo: r.Repo, Tag: r.Tag, Reason: r.Reason})
		}
		return json.NewEncoder(w).Encode(out)
	}
	if len(due) == 0 {
		_, err := io.WriteString(w, "nothing due\n")
		return err
	}
	for _, r := range due {
		if _, err := fmt.Fprintf(w, "%s:%s — %s\n", r.Repo, r.Tag, r.Reason); err != nil {
			return err
		}
	}
	return nil
}

// runReap renders the reap verdict: the plan in both modes —
// rows read identically dry or armed, only the trailer says
// whether they were marked. Evaluation and marking live in
// keeper.Reap; this stays printing-only.
var planJSON bool

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show pending sweep candidates with reasons",
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		s := d.Store
		return runPlan(cmd.Context(), cmd.OutOrStdout(), s, planJSON)
	},
}

// runDiscardPlan reports the discard count. No dry-run: discarding
// previews nothing — plan already showed the rows. The marks drop in
// keeper.DiscardPlan; this stays reporting-only.
func init() {
	planCmd.Flags().BoolVar(&planJSON, "json", false, "Render candidates as JSON for piping")
	planCmd.AddCommand(planDiscardCmd, planAddCmd, planRemoveCmd)
	RootCmd.AddCommand(planCmd)
}
