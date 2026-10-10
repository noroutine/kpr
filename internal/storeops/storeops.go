package storeops

import (
	"context"

	"nrtn.dev/catalyst/kpr/internal/policy"
)

// Recorder tracks minted generations for keep-N: the one method
// minting needs after a verified proof. store.Store satisfies it;
// the use cases declare only this.
type Recorder interface {
	Record(ctx context.Context, r policy.Row) error
}
