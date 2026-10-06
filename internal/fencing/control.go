package fencing

import (
	"context"
	"time"

	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Control implements fence.Controller beside the Gate: Hold
// delegates to the lease file, Deny/Allow record posture
// transitions to the ring (signed kpr-edge). Enforcement itself
// never moves — the marker and the lease stay the only truth —
// so drivers record without the power to disagree. Ephemeral
// narration belongs to the caller: drivers voice their own
// output, the ring keeps shared history.
type Control struct {
	store.HoldFile
	Store fence.GateStore
	// Now sources time; nil means time.Now (tests pin it).
	Now func() time.Time
}

var _ fence.Controller = Control{}

// Hold engages the lease file and records hold_engage; the
// wrapped release records hold_release. A Hold that fails to
// engage records nothing — no lease, no record — and the caller
// refuses instead of collecting unfenced.
func (c Control) Hold(ctx context.Context, until time.Time) (func(), error) {
	release, err := c.HoldFile.Hold(ctx, until)
	if err != nil {
		return nil, err
	}
	c.record("hold_engage", "HOLD lease engaged: manifest writes wait out the armed collect")
	return func() {
		release()
		c.record("hold_release", "HOLD lease released: manifest writes flow again")
	}, nil
}

// Deny records a deny_engage transition: lock flipped the marker,
// this writes it into history.
func (c Control) Deny(ctx context.Context, reason string) {
	c.record("deny_engage", reason)
}

// Allow records a deny_release transition: unlock proved the
// store, this writes it into history.
func (c Control) Allow(ctx context.Context, reason string) {
	c.record("deny_release", reason)
}

// record writes one fence transition to the ring, signed
// kpr-edge; nil store skips it.
func (c Control) record(outcome, msg string) {
	if c.Store == nil {
		return
	}
	_ = c.Store.PushActivity(context.Background(), store.Outcome{
		Reason:  msg,
		Outcome: outcome,
		At:      c.now(),
		Actor:   "kpr-edge",
	})
}

func (c Control) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
