package keeper

import (
	"context"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/helpers/human"
)

// ClockCard is the configured time transport and the live skew
// as kpr sees it: the same source the mint paths check through,
// read here for display. Raw durations, not words — renderers
// format through human. Standing is decided here, at assembly:
// local is unchecked (never a fake zero skew), a failed probe
// unreachable, a skew past tolerance degraded.
type ClockCard struct {
	Standing  Standing      `json:"standing"`
	Method    string        `json:"method"`
	Server    string        `json:"server"`
	Skew      time.Duration `json:"skewNanos"`
	Tolerance time.Duration `json:"toleranceNanos"`
}

// SkewText voices the live offset as kpr sees it: the signed
// number when reachable, "unreachable" on a failed probe,
// "—" when unchecked. Both renderers read this — one wording,
// never two.
func (c ClockCard) SkewText() string {
	switch c.Standing {
	case StandingUnreachable:
		return "unreachable"
	case StandingUnchecked:
		return "—"
	default:
		return human.SignedOffset(c.Skew)
	}
}

// NoteText voices the card's bound: the tolerance behind an
// ok/degraded/unreachable verdict, "unchecked" for local.
// Both renderers read this — one wording, never two.
func (c ClockCard) NoteText() string {
	if c.Standing == StandingUnchecked {
		return "unchecked"
	}
	return "tolerance " + c.Tolerance.String()
}

// FetchClockCard voices the transport and the skew: local
// trusts the machine clock without probing, anything else
// reads the source. Callers bound the context — an unreachable
// source must read unreachable, never slow.
func FetchClockCard(ctx context.Context, src clock.Source, method clock.Method, server string, tolerance time.Duration) ClockCard {
	if method == clock.MethodLocal {
		return ClockCard{Standing: StandingUnchecked, Method: string(method), Server: "machine clock", Tolerance: tolerance}
	}
	card := ClockCard{Standing: StandingOK, Method: string(method), Server: server, Tolerance: tolerance}
	offset, err := src.Offset(ctx, server)
	if err != nil {
		card.Standing = StandingUnreachable
		return card
	}
	card.Skew = offset
	if offset > tolerance || offset < -tolerance {
		card.Standing = StandingDegraded
	}
	return card
}
