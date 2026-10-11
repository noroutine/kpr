package keeper

import (
	"context"
	"time"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// ProofCard is the live same-store proof state: the generation
// the registry serves, read back through the API on every
// fetch. No generation served means no proof — unproven, never
// a guess. Standing is decided here, at assembly.
type ProofCard struct {
	Standing Standing `json:"standing"`
	Gen      string   `json:"gen,omitempty"`
	TS       string   `json:"ts,omitempty"`
}

// Proven reports whether the card voices a generation: the
// template's branch condition, derived from the standing so
// renderers never recompute the verdict.
func (p ProofCard) Proven() bool {
	return p.Standing == StandingOK
}

// Age reports how long ago the served generation was proven,
// through the payload's own arithmetic — one implementation,
// never a reparse per renderer.
func (p ProofCard) Age(now time.Time) (time.Duration, error) {
	return sentinel.Payload{Gen: p.Gen, TS: p.TS}.Age(now)
}

// FetchProofCard reads the live generation: a down registry or
// a read error renders unproven, nil API unconfigured. Callers
// bound the context — a dead source must never slow the fetch.
func FetchProofCard(ctx context.Context, api sentinel.API) ProofCard {
	if api == nil {
		return ProofCard{Standing: StandingClosed}
	}
	p, err := sentinel.LastProof(ctx, api)
	if err != nil {
		return ProofCard{Standing: StandingUnproven}
	}
	return ProofCard{Standing: StandingOK, Gen: p.Gen, TS: p.TS}
}
