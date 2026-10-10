package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/helpers/human"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/trust"
)

// identityState voices the lineage pairing in one line: the id the
// verdicts judge the served generation against, with the accepted
// baseline when one was recorded. Unpaired names itself; a read
// failure is unknown, never a guess.
func identityState(ctx context.Context, s store.Store) string {
	id, err := s.GetIdentity(ctx)
	if err != nil {
		return "unknown"
	}
	if id.ID == "" {
		return "unpaired"
	}
	if id.BaselineGen != "" {
		return id.ID + " (baseline " + id.BaselineGen + ")"
	}
	return id.ID
}

// activityTail is how many ring records the text view shows: the
// newest slice, with the total beside it. The full ring (at most
// ActivityCap small records) goes to --json.
const activityTail = 10

type activityJSON struct {
	Repo    string `json:"repo"`
	Tag     string `json:"tag"`
	Reason  string `json:"reason"`
	Outcome string `json:"outcome"`
	At      string `json:"at"`
	Actor   string `json:"actor,omitempty"`
	Trigger string `json:"trigger,omitempty"`
}

// runStoreStatus prints the store card the banner gave up: backend,
// intent, live proof, pairing, and the activity tail. A read failure
// on the ring degrades to unknown — the card above it already
// answered.
func runStoreStatus(ctx context.Context, w io.Writer, s store.Store, api sentinel.API, storeLine string, asJSON bool) error {
	status := "unproven"
	if rows, rerr := s.All(ctx); rerr == nil {
		if id, ierr := s.GetIdentity(ctx); ierr == nil {
			status = trust.Word(ctx, api, id, rows)
		}
	}
	lock := lockState(ctx, s)
	proof := proofState(ctx, api)
	ident := identityState(ctx, s)
	acts, actErr := s.Activity(ctx)
	if asJSON {
		out := struct {
			Status   string         `json:"status"`
			Store    string         `json:"store"`
			Lock     string         `json:"lock"`
			Proof    string         `json:"proof"`
			Identity string         `json:"identity"`
			Activity []activityJSON `json:"activity"`
		}{Status: status, Store: storeLine, Lock: lock, Proof: proof, Identity: ident}
		// NOTE(mutants): == is equivalent — every backend returns
		// nil rows with a read error (redis/file) or never errors
		// (mem), so ranging on the error path appends nothing either
		// way. A test feeding rows+error together would enshrine a
		// combination no backend produces.
		if actErr == nil {
			for _, a := range acts {
				out.Activity = append(out.Activity, activityJSON{Repo: a.Repo, Tag: a.Tag,
					Reason: a.Reason, Outcome: a.Outcome, At: a.At.UTC().Format(time.RFC3339),
					Actor: a.Actor, Trigger: a.Trigger})
			}
		}
		return json.NewEncoder(w).Encode(out)
	}
	if _, err := fmt.Fprintf(w, "status: %s\nstore: %s\nstore-lock: %s\nproof: %s\nidentity: %s\n",
		status, storeLine, lock, proof, ident); err != nil {
		return err
	}
	if actErr != nil {
		_, err := io.WriteString(w, "activity: unknown\n")
		return err
	}
	if len(acts) == 0 {
		_, err := io.WriteString(w, "activity: none recorded\n")
		return err
	}
	n := min(len(acts), activityTail)
	if _, err := fmt.Fprintf(w, "activity (last %d of %d):\n", n, len(acts)); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, a := range acts[:n] {
		if _, err := fmt.Fprintln(w, renderOutcome(now, a)); err != nil {
			return err
		}
	}
	return nil
}

// renderOutcome prints one ring entry: rows as repo:tag —
// outcome, row-less control events (fence flips) as the outcome
// alone, each with a human age (precise stamps stay in --json).
// A stray ": — " prefix reads as a malformed row.
func renderOutcome(now time.Time, a store.Outcome) string {
	age := "unknown age"
	if !a.At.IsZero() {
		age = human.ShortAge(now, a.At)
	}
	if a.Repo == "" && a.Tag == "" {
		return fmt.Sprintf("  %s (%s), %s", a.Outcome, a.Reason, age)
	}
	return fmt.Sprintf("  %s:%s — %s (%s), %s", a.Repo, a.Tag, a.Outcome, a.Reason, age)
}

var storeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show trust, backend, lock, proof, identity, and activity",
	Long: `The store card: trust word, backend, lock, live proof with
age, lineage pairing, and the activity tail.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		return runStoreStatus(cmd.Context(), cmd.OutOrStdout(), d.Store, d.Reg, describeStore(d.Store, d.Cfg), asJSON)
	},
}

func init() {
	storeStatusCmd.Flags().Bool("json", false, "Render the card as JSON")
	storeCmd.AddCommand(storeStatusCmd)
}
