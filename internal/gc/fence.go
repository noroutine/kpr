package gc

import (
	"fmt"
	"io"

	"nrtn.dev/catalyst/kpr/internal/fence"
)

// FenceForBackend decides the run's fence: the file backend
// shares the lease dir with the edge, anything else runs
// unfenced with the warning said out loud. The adapter arrives
// via newControl — the edge owns the adapter package, gc owns
// the decision, and neither imports the other for it (the
// leaseReady the preflight takes is simply this returning
// non-nil). Previews stay silent either way — nothing is
// deleted, so nothing holds.
func FenceForBackend(backend, dir string, newControl func(dir string) fence.Controller, backendErr error, dryRun bool, out io.Writer) fence.Controller {
	if backendErr == nil && backend == "file" {
		return newControl(dir)
	}
	if !dryRun {
		_, _ = fmt.Fprintln(out, "Warning: proxy HOLD fence unavailable without a shared file store — armed collect runs unfenced")
	}
	return nil
}
