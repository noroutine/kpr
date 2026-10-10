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
// collection and read per manifest PUT — and the redis key when
// the lease rides redis. The name is a cross-process contract —
// both processes plus operators — hence exported. It carries an
// expiry, never authority: stale or corrupt leases fail open.
const HoldFileName = "edge-fence.json"

// leasePayload is the one HOLD encoding, both media: file writes
// it, redis SETs it, every reader parses it. One payload, never
// per-backend dialects — a backend lands when it passes the
// shared conformance, which is only possible because the
// semantics turned out identical.
type leasePayload struct {
	Until time.Time `json:"until"`
}

// encodeLease renders the payload; marshal cannot fail on a
// struct of one time, so callers spend no branch on it.
func encodeLease(until time.Time) []byte {
	raw, _ := json.Marshal(leasePayload{Until: until.UTC()})
	return raw
}

// parseLease reads the payload back: the expiry plus whether it
// parses at all. Missing and corrupt leases read as absent (fail
// open — leases are not evidence); expiry is the caller's
// decision, so released (gone) and overrun (present but past)
// stay distinguishable.
func parseLease(raw []byte) (until time.Time, present bool) {
	var lease leasePayload
	if err := json.Unmarshal(raw, &lease); err != nil {
		return time.Time{}, false
	}
	return lease.Until, true
}

// FileLease engages proxy HOLD leases from a shared dir: gc
// writes the lease around armed collection, the edge reads it
// per manifest PUT. It implements the fence lease port from the
// file side; the registry side stays a stock binary. The file
// semantics below stay frozen — new backends conform to them,
// not the other way around.
type FileLease struct {
	Dir string
}

// Hold writes a lease expiring at until and returns its release
// (best-effort remove — the expiry is the real bound, so a
// crashed collect can never wedge pushes past it). The content
// lands through writeAtomic (temp file plus rename — readers see
// old or new, never partial); missing dirs refuse instead of
// being built, so gc hears the failure.
func (h FileLease) Hold(_ context.Context, until time.Time) (func(), error) {
	raw := encodeLease(until)
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
func (h FileLease) sweepStaleTemps() {
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
// at all. Missing and corrupt leases read as absent (fail open).
func (h FileLease) Read() (until time.Time, present bool) {
	if h.Dir == "" {
		return time.Time{}, false
	}
	raw, err := os.ReadFile(filepath.Join(h.Dir, HoldFileName))
	if err != nil {
		return time.Time{}, false
	}
	return parseLease(raw)
}

// HeldUntil returns the live lease expiry: only a present lease
// still in its term holds.
func (h FileLease) HeldUntil(now time.Time) (time.Time, bool) {
	until, present := h.Read()
	if !present || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}
