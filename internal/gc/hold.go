package gc

import (
	"context"
	"time"
)

// Fencer engages a proxy HOLD lease around armed collection.
// Nil fence (the default) collects unfenced — previews never
// engage, and deployments without a shared file store have no
// lease to write. The lease carries its own expiry, so a crashed
// collect can never wedge pushes past it.
type Fencer interface {
	Hold(ctx context.Context, until time.Time) (release func(), err error)
}
