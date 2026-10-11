package keeper

import (
	"context"

	"nrtn.dev/catalyst/kpr/internal/store"
)

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
