package resources

import (
	"sync"
)

// Holder is the live config: built once at boot (from the dir
// or empty when unconfigured), swapped whole on reload.
// Readers share snapshots without locking; only the swap
// locks. A failed reload keeps serving the old snapshot and
// records the error — never half-applied, never silent.
type Holder struct {
	mu      sync.RWMutex
	snap    *Snapshot
	load    func() (*Snapshot, error)
	lastErr error
}

// NewHolder takes the boot snapshot and the loader reload
// re-runs: the initial load already succeeded (boot refuses
// otherwise), so this never fails.
func NewHolder(snap *Snapshot, load func() (*Snapshot, error)) *Holder {
	return &Holder{snap: snap, load: load}
}

// Get returns the current snapshot for readers.
func (h *Holder) Get() *Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snap
}

// Check re-runs the loader without swapping: callers reconcile
// the candidate before Swap, so a reload either applies whole
// or not at all. A nil error state rides along — the snapshot
// behind it loaded clean.
func (h *Holder) Check() (*Snapshot, error) {
	next, err := h.load()
	if err != nil {
		h.mu.Lock()
		h.lastErr = err
		h.mu.Unlock()
		return nil, err
	}
	return next, nil
}

// Swap installs a snapshot Check loaded: the same whole-swap
// as Reload, minus the load. A nil error state rides along.
func (h *Holder) Swap(next *Snapshot) {
	h.mu.Lock()
	h.snap = next
	h.lastErr = nil
	h.mu.Unlock()
}

// Reload re-runs the loader and swaps on success. On failure
// the old snapshot keeps serving and the error waits on
// LoadErr for the dashboard and logs.
func (h *Holder) Reload() error {
	next, err := h.Check()
	if err != nil {
		return err
	}
	h.Swap(next)
	return nil
}

// LoadErr reports the last reload failure, nil when the live
// snapshot loaded clean — or when no reload ran yet.
func (h *Holder) LoadErr() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastErr
}
