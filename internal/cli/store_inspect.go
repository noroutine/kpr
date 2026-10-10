package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// runStoreInspect prints the full row for one exact repo:tag. An
// untracked spelling refuses naming it — never a near miss.
func runStoreInspect(ctx context.Context, w io.Writer, s store.Store, ref string, asJSON bool) error {
	repo, tag, err := splitRef(ref)
	if err != nil {
		return err
	}
	rows, err := s.All(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Repo != repo || r.Tag != tag {
			continue
		}
		if asJSON {
			return json.NewEncoder(w).Encode(rowToJSON(r))
		}
		_, err := fmt.Fprintf(w, "repo: %s\ntag: %s\ndigest: %s\nmedia_type: %s\npushed_at: %s\nactor: %s\ndue: %v\nreason: %s\n",
			r.Repo, r.Tag, r.Digest, r.MediaType,
			r.PushedAt.UTC().Format(time.RFC3339), r.Actor, r.Due, r.Reason)
		return err
	}
	return fmt.Errorf("no tracked row %s:%s", repo, tag)
}

var storeInspectCmd = &cobra.Command{
	Use:   "inspect <repo:tag>",
	Short: "Show one full tracked row",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		return runStoreInspect(cmd.Context(), cmd.OutOrStdout(), d.Store, args[0], asJSON)
	},
}

func init() {
	storeInspectCmd.Flags().Bool("json", false, "Render the row as JSON")
	storeCmd.AddCommand(storeInspectCmd)
}
