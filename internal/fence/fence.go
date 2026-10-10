package fence

import (
	"context"
	"time"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// Controller is the fencing port: HOLD leases around collection
// and explicit DENY/ALLOW posture transitions. Two drivers: gc
// armed collect (Hold) and store lock/unlock (Deny/Allow).
//
// These ops are the loud transitions, not second truth.
// Enforcement stays single-sourced — the lock marker and the
// lease file — so an announcement can never disagree with the
// gate: Deny/Allow voice the flip at the moment of the call,
// best-effort over the ring (a lost announcement never changes
// posture), while a Hold that fails to engage refuses instead
// of collecting unfenced.
type Controller interface {
	// Hold engages a proxy HOLD lease expiring at until and
	// returns its release. Nil fence (the default) collects
	// unfenced; previews never engage.
	Hold(ctx context.Context, until time.Time) (release func(), err error)
	// Deny voices a deny_engage transition for reason.
	Deny(ctx context.Context, reason string)
	// Allow voices a deny_release transition for reason.
	Allow(ctx context.Context, reason string)
}

// GateStore names only what the fence reads: the marker for
// posture checks and the ring for shared transition history.
// Every backend satisfies it structurally; the narrow type
// keeps fence code from reaching past marker and ring.
type GateStore interface {
	IsUnlocked(ctx context.Context) (bool, error)
	PushActivity(ctx context.Context, o store.Outcome) error
}
