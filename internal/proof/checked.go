package proof

import (
	"context"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
)

// BoundedClock is a clock the bound was proven on: the exchange
// ran and skew sits inside tolerance, so minted timestamps mean
// something. Sealed like every evidence: the only inhabitant
// comes from the Checker, and the zero value is nil.
type BoundedClock interface {
	sealed()
}

type boundedClock struct{}

func (boundedClock) sealed() {}

// Checker proves the clock bound once per run. Refusals pass
// through untouched — clock.Check's own contract (skew refuses,
// dead source warns-and-proceeds) stays with the callers that
// interpret it; only a clean check mints.
type Checker struct {
	// Tolerance bounds acceptable skew; clock.Tolerance is the
	// production value.
	Tolerance time.Duration
}

// Check runs the bound: nil error mints, anything else returns
// as-is with no BoundedClock.
func (c Checker) Check(ctx context.Context, src clock.Source, server string) (BoundedClock, error) {
	if err := clock.Check(ctx, src, server, c.Tolerance); err != nil {
		return nil, err
	}
	return boundedClock{}, nil
}
