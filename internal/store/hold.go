package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HoldFileName is the HOLD lease file, written by gc around armed
// collection and read per manifest PUT. The name is a
// cross-process contract — both processes plus operators — hence
// exported; the file encoding stays unexported. It carries an
// expiry, never authority: stale or corrupt leases fail open.
const HoldFileName = "edge-fence.json"

// HoldFile engages proxy HOLD leases from a shared dir: gc
// writes the lease around armed collection, the edge reads it
// per manifest PUT. It implements the gc fence port from the
// file side; the registry side stays a stock binary.
//
// STORES_FUTURE: per-backend lease impls split from here when a
// redis-HOLD consumer arrives (redis: SET NX EX; mem can never
// implement it honestly — HOLD is cross-process). Until then the
// file semantics below stay frozen.
type HoldFile struct {
	Dir string
}

// Hold writes a lease expiring at until and returns its release
// (best-effort remove — the expiry is the real bound, so a
// crashed collect can never wedge pushes past it). The content
// lands through writeAtomic (temp file plus rename — readers see
// old or new, never partial); missing dirs refuse instead of
// being built, so gc hears the failure.
func (h HoldFile) Hold(_ context.Context, until time.Time) (func(), error) {
	raw, err := json.Marshal(struct {
		Until time.Time `json:"until"`
	}{Until: until.UTC()})
	if err != nil {
		return nil, fmt.Errorf("edge: marshal hold lease: %w", err)
	}
	h.sweepStaleTemps()
	if err := writeAtomic(filepath.Join(h.Dir, HoldFileName), raw, ".edge-fence-*.tmp"); err != nil {
		return nil, fmt.Errorf("edge: write hold lease: %w", err)
	}
	return func() {
		_ = os.Remove(filepath.Join(h.Dir, HoldFileName))
	}, nil
}

// sweepStaleTemps removes temp files crashed writers left behind.
// Hold runs rarely (around gc), so the readdir costs nothing; an
// empty dir skips it, so marker-only gates never touch the cwd.
// Names match on the base only, so metacharacters in Dir can't
// escape the sweep. No age check: a second concurrent Hold would
// lose its temp and fail the rename loudly, never silently —
// today gc is the only caller, so that never happens.
func (h HoldFile) sweepStaleTemps() {
	if h.Dir == "" {
		return
	}
	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ok, _ := filepath.Match(".edge-fence-*.tmp", e.Name()); ok {
			_ = os.Remove(filepath.Join(h.Dir, e.Name()))
		}
	}
}

// Read parses the HOLD lease: the expiry plus whether it parses
// at all. Missing and corrupt leases read as absent (fail open —
// leases are not evidence); expiry is the caller's decision, so
// released (gone) and overrun (present but past) stay
// distinguishable.
func (h HoldFile) Read() (until time.Time, present bool) {
	if h.Dir == "" {
		return time.Time{}, false
	}
	raw, err := os.ReadFile(filepath.Join(h.Dir, HoldFileName))
	if err != nil {
		return time.Time{}, false
	}
	var lease struct {
		Until time.Time `json:"until"`
	}
	if err := json.Unmarshal(raw, &lease); err != nil {
		return time.Time{}, false
	}
	return lease.Until, true
}

// HeldUntil returns the live lease expiry: only a present lease
// still in its term holds.
func (h HoldFile) HeldUntil(now time.Time) (time.Time, bool) {
	until, present := h.Read()
	if !present || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}
