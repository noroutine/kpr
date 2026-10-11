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
// backend, translated state, intent, size when the card voices
// it, then the counters with the proof voice in banner words —
// the same facts the console's Store section shows. The frozen
// `store status` is the reference; this reads the model both
// renderers sign. A nil registry reads unproven with no probe,
// never a missing line.
func runStatusStore(ctx context.Context, w io.Writer, s store.Store, redisAddr string, redisDB int, reg statusRegistry, asJSON bool) error {
	st := keeper.FetchStatus(ctx, s, reg)
	proven := keeper.FetchProofCard(ctx, reg).Proven()
	card := keeper.FetchStoreCard(ctx, s, st.StoreOK, redisAddr, redisDB, proven)
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
	// The pairing verdicts judge the served generation against;
	// empty on no store, so the line stays out then.
	if card.Identity != "" {
		if _, err := fmt.Fprintf(w, "identity: %s\n", card.Identity); err != nil {
			return err
		}
	}
	if card.FsNote != "" {
		if _, err := fmt.Fprintf(w, "fs: %s\n", card.FsNote); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "tracked: %d\ndue: %d\nperformed: %d\nplanned: %d\nfailed: %d\nproof: %s\n",
		st.Tracked, st.Due, st.Performed, st.Planned, st.Failed, proofState(ctx, reg))
	return err
}

var statusStoreCmd = &cobra.Command{
	Use:   "store",
	Short: "Show the store card",
	Long: `Backend, state, intent, and size — the store card both
the console and the CLI render.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		cfg := config.Current()
		return runStatusStore(cmd.Context(), cmd.OutOrStdout(), d.Store,
			cfg.RedisAddr, cfg.RedisDB, d.Reg, asJSON)
	},
}

func init() {
	statusStoreCmd.Flags().Bool("json", false, "Render the card as JSON")
	statusCmd.AddCommand(statusStoreCmd)
}
