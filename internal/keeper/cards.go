package keeper

// Standing is a card's machine-readable state, set once at
// assembly. Presentations translate it each in their own way
// (console bulb class, CLI loud line, --json verbatim) —
// neither renderer recomputes the verdict from raw facts.
type Standing string

const (
	// StandingOK reads calm everywhere.
	StandingOK Standing = "ok"
	// StandingHeld pins writes under a hold lease.
	StandingHeld Standing = "held"
	// StandingDenied is the fence refusing traffic (edge DENY).
	// The store card never sets it — operator intent there is
	// StandingLocked.
	StandingDenied Standing = "denied"
	// StandingDegraded answers, below full health (a bound
	// crossed, a reading failed).
	StandingDegraded Standing = "degraded"
	// StandingUnreachable never answered the probe.
	StandingUnreachable Standing = "unreachable"
	// StandingUnproven has no proof to voice.
	StandingUnproven Standing = "unproven"
	// StandingLocked denies by operator intent.
	StandingLocked Standing = "locked"
	// StandingClosed is configured off, not broken.
	StandingClosed Standing = "closed"
	// StandingUnavailable has no backend to ask.
	StandingUnavailable Standing = "unavailable"
	// StandingUnchecked is not a verdict (local clock).
	StandingUnchecked Standing = "unchecked"
)
