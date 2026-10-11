package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// storeState translates the generic standing code into
// store-specific words: what the state means for this backend.
// --json keeps the raw code for machines. A standing this
// switch never learned voices its code rather than hiding a
// state nobody worded — silence would read as healthy.
func storeState(standing keeper.Standing) string {
	switch standing {
	case keeper.StandingOK:
		return "ready"
	case keeper.StandingLocked:
		return "locked — writes denied, reads continue"
	case keeper.StandingDegraded:
		return "degraded — lock intent unreadable"
	case keeper.StandingUnreachable:
		return "unreachable — backend not answering"
	case keeper.StandingUnavailable:
		return "unavailable — no store wired"
	default:
		return string(standing)
	}
}

// runStatusStore prints the store card from the shared model:
// backend, translated state, intent, size when the card voices
// it, then the counters with the proof voice in banner words —
// the same facts the console's Store section shows. The proof
// is fetched once and voiced twice (text and json share the
// fetch, never re-prove the registry). A nil registry reads
// unproven with no probe, never a missing line.
func runStatusStore(ctx context.Context, w io.Writer, s store.Store, redisAddr string, redisDB int, reg statusRegistry, asJSON bool) error {
	st := keeper.FetchStatus(ctx, s, reg)
	proof := keeper.FetchProofCard(ctx, reg)
	card := keeper.FetchStoreCard(ctx, s, st.StoreOK, redisAddr, redisDB, proof.Proven())
	if asJSON {
		// The machine view carries what the text carries: the
		// card with the counters and the proof voice beside
		// them — never a subset the text outgrows.
		out := struct {
			keeper.StoreCard
			Tracked   int    `json:"tracked"`
			Due       int    `json:"due"`
			Performed int    `json:"performed"`
			Planned   int    `json:"planned"`
			Failed    int    `json:"failed"`
			Proof     string `json:"proof"`
			Trust     string `json:"trust"`
		}{StoreCard: card,
			Tracked: st.Tracked, Due: st.Due,
			Performed: st.Performed, Planned: st.Planned, Failed: st.Failed,
			Proof: voiceProof(proof)}
		rows, _ := s.All(ctx)
		ident, _ := s.GetIdentity(ctx)
		out.Trust = keeper.FetchTrustCard(ctx, reg, ident, rows).Word
		return json.NewEncoder(w).Encode(out)
	}
	lock := card.LockNote
	if lock == "" {
		lock = "unlocked"
	}
	// One assembled write, like the aggregate: a broken pipe
	// errors with no partial card printed as success.
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "store: %s (%s)\nstate: %s\nlock: %s\n",
		card.Name, card.Detail, storeState(card.Standing), lock)
	// The pairing verdicts judge the served generation against;
	// empty on no store, so the line stays out then.
	if card.Identity != "" {
		_, _ = fmt.Fprintf(&sb, "identity: %s\n", card.Identity)
	}
	if card.FsNote != "" {
		_, _ = fmt.Fprintf(&sb, "fs: %s\n", card.FsNote)
	}
	_, _ = fmt.Fprintf(&sb, "tracked: %d\ndue: %d\nperformed: %d\nplanned: %d\nfailed: %d\nproof: %s\n",
		st.Tracked, st.Due, st.Performed, st.Planned, st.Failed, voiceProof(proof))
	// The trust verdict judges the served generation against the
	// local rows and pairing — the same inputs the old `store
	// status` judged, so the word survives the drop. The word
	// voices as-is: renderers never translate it.
	rows, _ := s.All(ctx)
	ident, _ := s.GetIdentity(ctx)
	_, _ = fmt.Fprintf(&sb, "trust: %s\n", keeper.FetchTrustCard(ctx, reg, ident, rows).Word)
	_, err := io.WriteString(w, sb.String())
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
