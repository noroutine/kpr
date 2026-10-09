package storeops

import (
	"context"
	"fmt"
	"io"

	"nrtn.dev/catalyst/kpr/internal/fence"
)

// LockStore is the store behind the lock marker plus the fence it
// voices through: every backend satisfies both in production, tests
// stub the half they break.
type LockStore interface {
	unlockStore
	fence.GateStore
}

// Lock drops the unlock marker: gc and every future store writer
// refuse until unlock proves the shared store again. The marker
// denies; the fence voices it — the ring carries deny_engage at
// the transition, not at the first refused push. Reads, sweeps,
// and the receiver keep working.
func Lock(ctx context.Context, w io.Writer, st LockStore) error {
	if err := st.SetUnlocked(ctx, false); err != nil {
		return err
	}
	fence.Control{Store: st}.Deny(ctx, "store locked: registry-store writes denied until 'kpr store unlock'")
	_, err := fmt.Fprintln(w, "store locked: registry-store writes denied until 'kpr store unlock'")
	return err
}
