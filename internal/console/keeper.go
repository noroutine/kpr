package console

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/helpers/human"
	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// probeTimeout bounds backend probes behind the dashboard: a down
// redis or registry makes the banner red, never the page slow.
// NOTE(mutants): arithmetic here only moves the deadline — a hung
// backend is indistinguishable from a slow one inside a unit run, so
// no test observes the bound. The red-banner tests pin the logic.
const probeTimeout = 2 * time.Second

// keeperData is everything the console shows about what kpr tracks —
// never a registry catalog: banner, counters, plan, activity.
type keeperData struct {
	RedisOK    bool
	RegistryOK bool
	Tracked    int
	Due        int
	Performed  int
	Planned    int
	Failed     int
	Untracked  int
	Plan       []keeperPlanRow
	// Activity is the capped tail (CLI status shape); ActivityTotal
	// is the uncapped ring size, so the cap reads honest. Full
	// precise records live behind /api/activity.
	Activity      []keeperActivityRow
	ActivityTotal int
	// EdgeOpen renders the gateway section open (proven and
	// serving); EdgeDeny/EdgeHeld are its live fence posture —
	// what IS, while the activity ring shows what CHANGED.
	EdgeOpen bool
	EdgeDeny bool
	EdgeHeld bool
	// RegistryURL/EdgeAddr name the endpoints the cards talk
	// about (edge forwards EdgeAddr → RegistryURL); empty renders
	// nothing, never a guess.
	RegistryURL string
	EdgeAddr    string
	// ClockMethod/ClockServer voice the configured time transport;
	// ClockSkew is the live offset as kpr sees it ("+83ms",
	// "unreachable", or "—" when unchecked), ClockNote its bound.
	// ClockNow is the server clock at render (what "now" means to
	// kpr). The skew reading is taken during the same render, so a
	// separate checked stamp would always read "just now" — noise,
	// not information. (The proof itself carries no obtained-at.)
	ClockMethod string
	ClockServer string
	ClockSkew   string
	ClockNote   string
	ClockNow    string
}

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

// keeperSnapshot renders the keeper use case for the dashboard. The
// numbers come from keeper.FetchStatus — the same implementation
// `status` reads; this stays formatting-only, degrading to red/empty
// when a backend is absent or down.
func (s *Server) keeperSnapshot(ctx context.Context) keeperData {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	st := keeper.FetchStatus(ctx, s.Store, s.Registry)
	k := keeperData{
		RedisOK:    st.StoreOK,
		RegistryOK: st.RegistryOK,
		Tracked:    st.Tracked, Due: st.Due,
		Performed: st.Performed, Planned: st.Planned,
		Failed: st.Failed, Untracked: st.Untracked,
	}
	if s.Edge != nil {
		k.EdgeOpen = true
		k.EdgeDeny, k.EdgeHeld = s.Edge.Snapshot()
		// Would-deny, not just last-denied: a fresh lock with no
		// traffic yet still reads deny, so the operator sees the
		// posture before the first refused push, not after. An
		// unreadable marker reads deny — the fence refuses then
		// too.
		if s.Store != nil {
			if unlocked, err := s.Store.IsUnlocked(ctx); err != nil || !unlocked {
				k.EdgeDeny = true
			}
		}
	}
	k.RegistryURL, k.EdgeAddr = s.RegistryURL, s.EdgeAddr
	now := time.Now().UTC()
	k.ClockNow = now.Format("2006-01-02 15:04:05 UTC")
	k.ClockMethod, k.ClockServer, k.ClockSkew, k.ClockNote = clockSnapshot(ctx)
	for _, p := range st.Plan {
		pushed := p.PushedAt.UTC().Format(time.RFC3339)
		if p.PushedAt.IsZero() {
			pushed = "unknown"
		}
		k.Plan = append(k.Plan, keeperPlanRow{Repo: p.Repo, Tag: p.Tag, Reason: p.Reason, Pushed: pushed})
	}
	k.ActivityTotal = len(st.Activity)
	for _, a := range st.Activity {
		if len(k.Activity) >= activityTail {
			break
		}
		k.Activity = append(k.Activity, keeperActivityRow{Line: activityLine(a)})
	}
	return k
}

// activityTail mirrors the CLI status view: the console shows the
// same last-10 tail, capped and said out loud.
const activityTail = 10

// activityLine formats one ring entry exactly like `store status`
// (see renderOutcome in internal/cli): row-less control events
// without the repo:tag prefix, every line with a human age.
// Friendly only — precise stamps live behind /api/activity.
func activityLine(a keeper.ActivityEntry) string {
	age := humanAge(a.At)
	// No leading indent: the CLI pads for the terminal, the card
	// has its own padding.
	if a.Repo == "" && a.Tag == "" {
		return fmt.Sprintf("%s (%s), %s", a.Outcome, a.Reason, age)
	}
	return fmt.Sprintf("%s:%s — %s (%s), %s", a.Repo, a.Tag, a.Outcome, a.Reason, age)
}

// humanAge renders a ring timestamp the way the CLI's shortAge
// does: glanceable ("5m ago") beside the exact stamp, never
// instead of it. A zero or future stamp degrades to words.
func humanAge(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	d := time.Since(at)
	// NOTE(mutants): < is equivalent — the two sides differ only
	// at exactly zero, where the clamp body is identity anyway.
	if d < 0 {
		d = 0
	}
	return human.Dur(d.Round(time.Second)) + " ago"
}

// clockSnapshot voices the configured time transport and the live
// skew as kpr sees it: the same source the mint paths check
// through, read here for display. An unreachable source reads
// "unreachable" (never slow: the caller's probe timeout bounds
// it); local trusts the machine clock, so skew is "—".
func clockSnapshot(ctx context.Context) (method, server, skew, note string) {
	cfg := config.Current()
	method = string(cfg.TimeMethod)
	server = cfg.TimeServer
	if cfg.TimeMethod == clock.MethodLocal {
		return method, "machine clock", "—", "unchecked"
	}
	src := clockSourceFor(cfg.TimeMethod)
	offset, err := src.Offset(ctx, server)
	if err != nil {
		return method, server, "unreachable", "tolerance " + clock.Tolerance.String()
	}
	return method, server, formatOffset(offset),
		"tolerance " + clock.Tolerance.String()
}

// formatOffset voices a clock skew with its sign: ahead reads +,
// behind reads -. If this fails, the card flips behind/ahead.
func formatOffset(offset time.Duration) string {
	sign := "+"
	if offset < 0 {
		sign = "-"
	}
	return sign + offset.Round(time.Millisecond).Abs().String()
}

// clockSourceFor mirrors the CLI composition root (see clockSource
// in internal/cli): same method, same transport, no second wiring.
func clockSourceFor(m clock.Method) clock.Source {
	switch m {
	case clock.MethodNTP:
		return clock.NTP{}
	case clock.MethodHTTPS:
		return clock.HTTPS{}
	default:
		return clock.Local{}
	}
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
