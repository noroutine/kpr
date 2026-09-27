package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"
)

// probeTimeout bounds backend probes behind the dashboard: a down
// redis or registry makes the banner red, never the page slow.
const probeTimeout = 2 * time.Second

// keeperData is everything the console shows about what kpr tracks —
// never a registry catalog: banner, counters, plan, activity.
type keeperData struct {
	RedisOK    bool
	RegistryOK bool
	Armed      bool
	Tracked    int
	Due        int
	Performed  int
	Planned    int
	Failed     int
	Untracked  int
	Plan       []keeperPlanRow
	Activity   []keeperActivityRow
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
}

// keeperSnapshot reads the tracked state, degrading to red/empty when
// a backend is absent or down.
func (s *Server) keeperSnapshot(ctx context.Context) keeperData {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var k keeperData
	k.Armed = s.Armed
	if s.Store != nil {
		k.RedisOK = s.Store.Ping(ctx) == nil
	}
	if s.Registry != nil {
		k.RegistryOK = s.Registry.Reachable(ctx) == nil
	}
	if s.Store == nil || !k.RedisOK {
		return k
	}
	rows, err := s.Store.All(ctx)
	if err != nil {
		k.RedisOK = false
		return k
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		return rows[i].Tag < rows[j].Tag
	})
	k.Tracked = len(rows)
	for _, r := range rows {
		if !r.Due {
			continue
		}
		k.Due++
		pushed := r.PushedAt.UTC().Format(time.RFC3339)
		if r.PushedAt.IsZero() {
			pushed = "unknown"
		}
		k.Plan = append(k.Plan, keeperPlanRow{Repo: r.Repo, Tag: r.Tag, Reason: r.Reason, Pushed: pushed})
	}
	acts, err := s.Store.Activity(ctx)
	if err != nil {
		return k
	}
	for _, a := range acts {
		switch a.Outcome {
		case "deleted":
			k.Performed++
		case "planned":
			k.Planned++
		case "failed":
			k.Failed++
		case "untracked":
			k.Untracked++
		}
		at := a.At.UTC().Format(time.RFC3339)
		if a.At.IsZero() {
			at = "unknown"
		}
		k.Activity = append(k.Activity, keeperActivityRow{
			Repo: a.Repo, Tag: a.Tag, Reason: a.Reason, Outcome: a.Outcome, At: at,
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
