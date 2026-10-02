package web

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
const probeTimeout = 2 * time.Second

// keeperData is everything the console shows about what kpr tracks —
// never a registry catalog: banner, counters, plan, activity.
type keeperData struct {
	RedisOK bool
	// BackendLabel names the banner card for the wired backend
	// ("Redis", "File store") — the label used to lie on non-redis
	// stacks, where every backend answered under Redis's name.
	BackendLabel string
	RegistryOK   bool
	Armed        bool
	Tracked      int
	Due          int
	Performed    int
	Planned      int
	Failed       int
	Untracked    int
	Plan         []keeperPlanRow
	Activity     []keeperActivityRow
	// EdgeOpen renders the gateway section open (proven and
	// serving); EdgeDeny/EdgeHeld are its live fence posture —
	// what IS, while the activity ring shows what CHANGED.
	EdgeOpen bool
	EdgeDeny bool
	EdgeHeld bool
}

type keeperPlanRow struct {
	Repo   string
	Tag    string
	Reason string
	Pushed string
}

type keeperActivityRow struct {
	Repo    string
	Tag     string
	Reason  string
	Outcome string
	At      string
	Actor   string
	Trigger string
}

// keeperSnapshot renders the keeper use case for the dashboard. The
// numbers come from keeper.FetchStatus — the same implementation
// `status` reads; this stays formatting-only, degrading to red/empty
// when a backend is absent or down.
func (s *Server) keeperSnapshot(ctx context.Context) keeperData {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	st := keeper.FetchStatus(ctx, s.Store, s.Registry)
	k := keeperData{
		RedisOK: st.StoreOK, BackendLabel: backendLabel(s.Store),
		RegistryOK: st.RegistryOK, Armed: s.Armed,
		Tracked: st.Tracked, Due: st.Due,
		Performed: st.Performed, Planned: st.Planned,
		Failed: st.Failed, Untracked: st.Untracked,
	}
	if s.Edge != nil {
		k.EdgeOpen = true
		k.EdgeDeny, k.EdgeHeld = s.Edge.Snapshot()
	}
	for _, p := range st.Plan {
		pushed := p.PushedAt.UTC().Format(time.RFC3339)
		if p.PushedAt.IsZero() {
			pushed = "unknown"
		}
		k.Plan = append(k.Plan, keeperPlanRow{Repo: p.Repo, Tag: p.Tag, Reason: p.Reason, Pushed: pushed})
	}
	for _, a := range st.Activity {
		at := a.At.UTC().Format(time.RFC3339)
		if a.At.IsZero() {
			at = "unknown"
		}
		k.Activity = append(k.Activity, keeperActivityRow{
			Repo: a.Repo, Tag: a.Tag, Reason: a.Reason, Outcome: a.Outcome, At: at,
			Actor: a.Actor, Trigger: a.Trigger,
		})
	}
	return k
}

// sweepHandler triggers one sweep pass and answers its summary as
// JSON — the synchronous feedback `kpr sweep` watches.
func (s *Server) sweepHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Sweeper == nil {
		http.Error(w, "sweeper not configured", http.StatusServiceUnavailable)
		return
	}
	sum := s.Sweeper.RunPass(r.Context(), "POST")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sum); err != nil {
		log.Printf("Error encoding JSON: %v", err)
	}
}
