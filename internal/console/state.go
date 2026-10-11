package console

import (
	"context"

	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// The store card now lives in keeper (spike milestone 1); the
// backend switch, lock note, and filesystem probe moved with
// their tests, not dropped.

// Filesystem probing now lives in keeper beside the store card
// (spike milestone 1); the guard truth tables moved with it, not
// dropped.

// sentinelSnapshot reads the live sentinel generation for the
// dashboard through the shared proof card: bounded like every
// other console probe, so a down registry renders unproven,
// never slow and never 500.
func (s *Server) sentinelSnapshot(ctx context.Context) keeper.ProofCard {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return keeper.FetchProofCard(ctx, s.Sentinel)
}
