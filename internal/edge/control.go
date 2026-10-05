package edge

import (
	"context"
	"time"

	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Control implements fence.Controller beside the Gate: Hold
// delegates to the lease file, Deny/Allow announce posture
// transitions through the same channels the Gate enforces on
// (report stages + ring outcomes, signed kpr-edge). Enforcement
// itself never moves — the marker and the lease stay the only
// truth — so drivers announce without the power to disagree.
type Control struct {
	HoldFile
	Store GateStore
	// Report receives transition events; nil discards.
	Report gc.Reporter
	// Now sources time; nil means time.Now (tests pin it).
	Now func() time.Time
}

var _ fence.Controller = Control{}

// Deny voices a deny_engage transition: lock flipped the marker,
// this says it out loud.
func (c Control) Deny(ctx context.Context, reason string) {
	announce(c.Store, c.Report, c.now(), StageDenyEngage, reason, "deny_engage")
}

// Allow voices a deny_release transition: unlock proved the
// store, this says it out loud.
func (c Control) Allow(ctx context.Context, reason string) {
	announce(c.Store, c.Report, c.now(), StageDenyRelease, reason, "deny_release")
}

func (c Control) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// announce delivers one fence transition: a stage event plus a
// ring outcome, never per request. Nil report discards the
// event, nil store skips the ring — a storeless transition
// still voices.
func announce(st GateStore, report gc.Reporter, now time.Time, stage, msg, outcome string) {
	gc.Emit(report, gc.Event{Stage: stage, Message: msg})
	if st == nil {
		return
	}
	_ = st.PushActivity(context.Background(), store.Outcome{
		Reason:  msg,
		Outcome: outcome,
		At:      now,
		Actor:   "kpr-edge",
	})
}
