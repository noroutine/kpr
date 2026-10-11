package console

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// probeTimeout bounds backend probes behind the dashboard: a down
// redis or registry makes the banner red, never the page slow.
// NOTE(mutants): arithmetic here only moves the deadline — a hung
// backend is indistinguishable from a slow one inside a unit run, so
// no test observes the bound. The red-banner tests pin the logic.
const probeTimeout = 2 * time.Second

// keeperPlanRow is one plan line shaped for the template: the
// push stamp preformatted, "unknown" when the store never said.
type keeperPlanRow struct {
	Repo   string
	Tag    string
	Reason string
	Pushed string
}

// keeperActivityRow is one preformatted ring line in the CLI
// status shape — the console shows the friendly view, /api/activity
// serves the precise records.
type keeperActivityRow struct {
	Line string
}

// planRows shapes the plan for the template: the push stamp
// preformatted, "unknown" when the store never said.
func planRows(plan []keeper.PlanEntry) []keeperPlanRow {
	var rows []keeperPlanRow
	for _, p := range plan {
		pushed := p.PushedAt.UTC().Format(time.RFC3339)
		if p.PushedAt.IsZero() {
			pushed = "unknown"
		}
		rows = append(rows, keeperPlanRow{Repo: p.Repo, Tag: p.Tag, Reason: p.Reason, Pushed: pushed})
	}
	return rows
}

// activityRows shapes the capped ring tail for the template. It
// mirrors the CLI status view through the shared formatter
// (unpadded — the card has its own padding): same last-10 tail,
// capped and said out loud.
func activityRows(entries []keeper.ActivityEntry, now time.Time) []keeperActivityRow {
	var rows []keeperActivityRow
	for _, a := range entries {
		if len(rows) >= keeper.ActivityTail {
			break
		}
		rows = append(rows, keeperActivityRow{Line: keeper.FormatOutcome(now, a, false)})
	}
	return rows
}

// activityHandler serves the full precise ring as JSON for download:
// the dashboard shows the friendly capped tail, this endpoint keeps
// every record with exact stamps. Nil store answers 503, never 500.
func (s *Server) activityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	acts, err := s.Store.Activity(ctx)
	if err != nil {
		http.Error(w, "activity unreadable", http.StatusServiceUnavailable)
		return
	}
	type record struct {
		Repo    string `json:"repo,omitempty"`
		Tag     string `json:"tag,omitempty"`
		Reason  string `json:"reason"`
		Outcome string `json:"outcome"`
		At      string `json:"at"`
		Actor   string `json:"actor,omitempty"`
		Trigger string `json:"trigger,omitempty"`
	}
	out := struct {
		Activity []record `json:"activity"`
	}{}
	for _, a := range acts {
		at := a.At.UTC().Format(time.RFC3339)
		if a.At.IsZero() {
			at = "unknown"
		}
		out.Activity = append(out.Activity, record{
			Repo: a.Repo, Tag: a.Tag, Reason: a.Reason, Outcome: a.Outcome,
			At: at, Actor: a.Actor, Trigger: a.Trigger,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("Error encoding JSON: %v", err)
	}
}
