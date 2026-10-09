package gc

import (
	"fmt"
	"io"

	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// holdDirStore is the lease-hosting capability: a store both
// processes see. Only the file backend has it today; mem and
// redis run unfenced, honestly, with the warning said out loud.
type holdDirStore interface {
	HoldDir() (string, bool)
}

// fenceForBackend decides the run's fence AND builds it from
// the store's own capability — no backend names travel here
// (the leaseReady the preflight takes is simply this returning
// non-nil). Previews stay silent either way — nothing is
// deleted, so nothing holds. Armed travels as the mint, never
// a bool: the signature is the point of proofs.
func fenceForBackend(st fence.GateStore, armed proof.ArmedRun, out io.Writer) fence.Controller {
	if hs, ok := st.(holdDirStore); ok {
		if dir, ok := hs.HoldDir(); ok {
			return fence.Control{HoldFile: store.HoldFile{Dir: dir}, Store: st}
		}
	}
	if !proof.Unarmed(armed) {
		_, _ = fmt.Fprintln(out, "Warning: proxy HOLD fence unavailable without a shared file store — armed collect runs unfenced")
	}
	return nil
}
