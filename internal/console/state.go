package console

import (
	"context"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// The store card now lives in keeper (spike milestone 1); the
// backend switch, lock note, and filesystem probe moved with
// their tests, not dropped.

// sentinelData is the live same-store proof state: the fixed
// sentinel address plus the generation the registry currently
// serves, read back through the API on every page load. No
// generation served means no proof — "unproven", never a guess.
type sentinelData struct {
	Gen    string
	TS     string
	Proven bool
}

// Filesystem probing now lives in keeper beside the store card
// (spike milestone 1); the guard truth tables moved with it, not
// dropped.

// sentinelSnapshot reads the live sentinel generation for the
// dashboard, bounded like every other console probe: a down
// registry or an unproven store renders unproven, never slow and
// never 500. Nil API renders unconfigured.
func (s *Server) sentinelSnapshot(ctx context.Context) sentinelData {
	var d sentinelData
	if s.Sentinel == nil {
		return d
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	p, err := sentinel.LastProof(ctx, s.Sentinel)
	if err != nil {
		return d
	}
	d.Gen, d.TS, d.Proven = p.Gen, p.TS, true
	return d
}
