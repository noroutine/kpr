package gc

import (
	"fmt"
	"io"

	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// FenceForBackend decides the run's fence AND builds it: the
// file backend shares the lease dir with the edge, anything
// else runs unfenced with the warning said out loud (the
// leaseReady the preflight takes is simply this returning
// non-nil). Previews stay silent either way — nothing is
// deleted, so nothing holds. Armed travels as the mint, never
// a bool: the signature is the point of proofs.
func FenceForBackend(backend, dir string, st fence.GateStore, armed proof.ArmedRun, out io.Writer) fence.Controller {
	if backend == "file" {
		return fence.Control{HoldFile: store.HoldFile{Dir: dir}, Store: st}
	}
	if !proof.Unarmed(armed) {
		_, _ = fmt.Fprintln(out, "Warning: proxy HOLD fence unavailable without a shared file store — armed collect runs unfenced")
	}
	return nil
}
