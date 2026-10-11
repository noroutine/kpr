package keeper

import (
	"context"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/trust"
)

// TrustCard voices the dry lineage verdict: the served
// generation judged against the local identity and rows. The
// word is the verdict's own (paired, unpaired, unserved,
// unproven, foreign, rollback?, behind); standing is decided
// here, at assembly — paired reads ok, anomalies degrade or
// deny, missing evidence never alarms. Renderers voice the
// word, never a translation of it.
type TrustCard struct {
	Standing Standing `json:"standing"`
	Word     string   `json:"word"`
}

// FetchTrustCard judges the served generation through the
// shared trust word and voices the card. Nil API reads what
// the word reads: unpaired on a fresh store, unproven
// otherwise — never a probe that never ran.
func FetchTrustCard(ctx context.Context, api sentinel.API, ident store.Identity, rows []policy.Row) TrustCard {
	word := trust.Word(ctx, api, ident, rows)
	switch word {
	case "paired":
		return TrustCard{Standing: StandingOK, Word: word}
	case "foreign":
		return TrustCard{Standing: StandingDenied, Word: word}
	case "rollback?", "behind":
		return TrustCard{Standing: StandingDegraded, Word: word}
	case "unserved":
		return TrustCard{Standing: StandingUnreachable, Word: word}
	default:
		return TrustCard{Standing: StandingUnproven, Word: word}
	}
}
