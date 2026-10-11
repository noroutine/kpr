package keeper

import (
	"context"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// PlanCard is the read side of plan add/rm: due rows with
// reasons, same rows the console lists. Standing voices the
// fetch, never the backlog — an empty plan reads ok, a down
// store unreachable.
type PlanCard struct {
	Standing Standing    `json:"standing"`
	Rows     []PlanEntry `json:"rows"`
}

// FetchPlanCard voices due rows through the shared counting:
// one implementation serves the console list and the CLI.
func FetchPlanCard(ctx context.Context, s store.Store, reg Prober) PlanCard {
	st := FetchStatus(ctx, s, reg)
	if !st.StoreOK {
		return PlanCard{Standing: StandingUnreachable}
	}
	return PlanCard{Standing: StandingOK, Rows: st.Plan}
}

// ActivityCard voices the ring with its honest total: the tail
// cap reads "last n of total", never a silent truncation.
// Standing voices the fetch — a down store reads unreachable.
type ActivityCard struct {
	Standing Standing        `json:"standing"`
	Outcomes []ActivityEntry `json:"outcomes"`
	Total    int             `json:"total"`
}

// FetchActivityCard voices the ring through the shared
// counting: one implementation serves the console tail, the
// CLI tail, and the full-ring JSON.
func FetchActivityCard(ctx context.Context, s store.Store) ActivityCard {
	st := FetchStatus(ctx, s, nil)
	if !st.StoreOK {
		return ActivityCard{Standing: StandingUnreachable}
	}
	return ActivityCard{Standing: StandingOK, Outcomes: st.Activity, Total: len(st.Activity)}
}
