package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// storeStateWords translates the generic standing code into
// store-specific words for the text view: what the state means
// for this backend. --json keeps the raw code for machines. A
// standing this switch never learned voices its code rather
// than hiding a state nobody worded — silence would read as
// healthy.
func storeStateWords(standing keeper.Standing) string {
	switch standing {
	case keeper.StandingOK:
		return "state: ready"
	case keeper.StandingLocked:
		return "state: locked — writes denied, reads continue"
	case keeper.StandingDegraded:
		return "state: degraded — lock intent unreadable"
	case keeper.StandingUnreachable:
		return "state: unreachable — backend not answering"
	case keeper.StandingUnavailable:
		return "state: unavailable — no store wired"
	default:
		return "state: " + string(standing)
	}
}

// runStatusStore prints the store card from the shared model:
// backend, translated state, intent, and size when the card
// voices it. The frozen `store status` is the reference; this
// reads the model both renderers sign.
func runStatusStore(ctx context.Context, w io.Writer, s store.Store, redisAddr string, redisDB int, proven, asJSON bool) error {
	healthy := s.Ping(ctx) == nil
	card := keeper.FetchStoreCard(ctx, s, healthy, redisAddr, redisDB, proven)
	if asJSON {
		return json.NewEncoder(w).Encode(card)
	}
	lock := card.LockNote
	if lock == "" {
		lock = "unlocked"
	}
	if _, err := fmt.Fprintf(w, "store: %s (%s)\n%s\nlock: %s\n",
		card.Name, card.Detail, storeStateWords(card.Standing), lock); err != nil {
		return err
	}
	if card.FsNote != "" {
		_, err := fmt.Fprintf(w, "fs: %s\n", card.FsNote)
		return err
	}
	return nil
}

var statusStoreCmd = &cobra.Command{
	Use:   "store",
	Short: "Show the store card from the shared status model",
	Long: `Backend, state, intent, and size — the store card both
the console and the CLI render.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		proven := false
		if d.Reg != nil {
			if _, err := sentinel.LastProof(cmd.Context(), d.Reg); err == nil {
				proven = true
			}
		}
		cfg := config.Current()
		return runStatusStore(cmd.Context(), cmd.OutOrStdout(), d.Store,
			cfg.RedisAddr, cfg.RedisDB, proven, asJSON)
	},
}

func init() {
	statusStoreCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusStoreCmd)
}
