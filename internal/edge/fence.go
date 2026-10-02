package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Control stages, beside the gc ones: same Event shape, same
// JSON-lines transport, edge-triggered on flips — never per
// request.
const (
	StageDenyEngage  = "deny_engage"
	StageDenyRelease = "deny_release"
	StageHoldEngage  = "hold_engage"
	StageHoldRelease = "hold_release"
)

// holdFileName is the HOLD lease file, written by gc around
// armed collection and read per manifest PUT. It carries an
// expiry, never authority: stale or corrupt leases fail open.
const holdFileName = "edge-fence.json"

// manifestRef matches reference-bearing registry paths: manifest
// PUT creates references, manifest DELETE destroys them. Blob
// uploads and every read pass the fence untouched.
var manifestRef = regexp.MustCompile(`^/v2/.+/manifests/.+$`)

// GateStore names only what the fence reads: the marker for
// DENY, the ring for flip outcomes. store.Store satisfies it
// structurally, so the gate never names the backend.
type GateStore interface {
	IsUnlocked(ctx context.Context) (bool, error)
	PushActivity(ctx context.Context, o store.Outcome) error
}

// Gate enforces HOLD/DENY around a proxied handler. DENY
// evaluates the lock through the same proof path the use cases
// mint from (token discarded — the fence needs a boolean, not
// a mint), so kpr's self-gating and the proxy's fencing cannot
// drift apart.
type Gate struct {
	Store GateStore
	// Dir holds the HOLD lease file; "" means no HOLD source
	// (leases need the shared file store).
	Dir string
	// Report receives flip events; nil discards.
	Report gc.Reporter
	// Now sources time; nil means time.Now (tests pin it).
	Now func() time.Time

	mu       sync.Mutex
	lastDeny bool
	lastHeld bool
}

// HoldFile engages proxy HOLD leases from a shared dir: gc
// writes the lease around armed collection, the edge reads it
// per manifest PUT. It implements the gc fence port from the
// file side; the registry side stays a stock binary.
type HoldFile struct {
	Dir string
}

// Hold writes a lease expiring at until and returns its release
// (best-effort remove — the expiry is the real bound, so a
// crashed collect can never wedge pushes past it).
func (h HoldFile) Hold(_ context.Context, until time.Time) (func(), error) {
	raw, err := json.Marshal(struct {
		Until time.Time `json:"until"`
	}{Until: until.UTC()})
	if err != nil {
		return nil, fmt.Errorf("edge: marshal hold lease: %w", err)
	}
	if err := os.WriteFile(filepath.Join(h.Dir, holdFileName), raw, 0o600); err != nil {
		return nil, fmt.Errorf("edge: write hold lease: %w", err)
	}
	return func() {
		_ = os.Remove(filepath.Join(h.Dir, holdFileName))
	}, nil
}

// heldUntil returns the live lease expiry: missing, corrupt, or
// past leases read as no hold (fail open — leases are not
// evidence).
func heldUntil(dir string, now time.Time) (time.Time, bool) {
	if dir == "" {
		return time.Time{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, holdFileName))
	if err != nil {
		return time.Time{}, false
	}
	var lease struct {
		Until time.Time `json:"until"`
	}
	if err := json.Unmarshal(raw, &lease); err != nil {
		return time.Time{}, false
	}
	if !lease.Until.After(now) {
		return time.Time{}, false
	}
	return lease.Until, true
}

// Wrap gates reference-bearing writes: HOLD delays to lease
// expiry (fail open), DENY refuses fast while locked. Blob
// uploads and reads forward untouched.
func (g *Gate) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isManifestWrite(r) {
			next.ServeHTTP(w, r)
			return
		}
		now := g.now()
		if until, held := heldUntil(g.Dir, now); held {
			g.flipHeld(true, "gc finalize holds manifest writes")
			g.waitRelease(r, until)
		}
		g.flipHeld(false, "hold lease over, edge re-evaluates")
		// No direct forward after a hold: the marker may have
		// flipped mid-lease (operator locks during gc), so every
		// held request faces the current evaluation on release.
		unlocked, err := proof.ProveUnlockedStore(r.Context(), g.Store)
		if err != nil {
			g.flipDeny(true, describeLockErr(err))
			if errors.Is(err, proof.ErrLocked) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusLocked)
				_, _ = fmt.Fprintln(w, "store locked: manifest writes denied until `kpr store unlock` proves the shared store")
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "edge cannot read the lock marker: %v\n", err)
			return
		}
		_ = unlocked // evaluation shared, token discarded
		g.flipDeny(false, "store unlocked, edge resumes")
		next.ServeHTTP(w, r)
	})
}

func isManifestWrite(r *http.Request) bool {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		return false
	}
	return manifestRef.MatchString(r.URL.Path)
}

// waitRelease sleeps to the lease expiry, fail open on client
// disconnect: a gone client holds nothing.
func (g *Gate) waitRelease(r *http.Request, until time.Time) {
	left := time.Until(until)
	if left <= 0 {
		return
	}
	t := time.NewTimer(left)
	defer t.Stop()
	select {
	case <-r.Context().Done():
	case <-t.C:
	}
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// flipHeld/flipDeny emit edge-triggered: one event plus one ring
// outcome per change, never per request.
func (g *Gate) flipHeld(held bool, msg string) {
	g.mu.Lock()
	flipped := held != g.lastHeld
	g.lastHeld = held
	g.mu.Unlock()
	if !flipped {
		return
	}
	stage := StageHoldRelease
	outcome := "hold_release"
	if held {
		stage, outcome = StageHoldEngage, "hold_engage"
	}
	g.emit(stage, msg, outcome)
}

func (g *Gate) flipDeny(deny bool, msg string) {
	g.mu.Lock()
	flipped := deny != g.lastDeny
	g.lastDeny = deny
	g.mu.Unlock()
	if !flipped {
		return
	}
	stage := StageDenyRelease
	outcome := "deny_release"
	if deny {
		stage, outcome = StageDenyEngage, "deny_engage"
	}
	g.emit(stage, msg, outcome)
}

func (g *Gate) emit(stage, msg, outcome string) {
	gc.Emit(g.Report, gc.Event{Stage: stage, Message: msg})
	if g.Store == nil {
		return
	}
	_ = g.Store.PushActivity(context.Background(), store.Outcome{
		Reason:  msg,
		Outcome: outcome,
		At:      g.now(),
		Actor:   "kpr-edge",
	})
}

func describeLockErr(err error) string {
	if errors.Is(err, proof.ErrLocked) {
		return "store locked: manifest writes denied"
	}
	return fmt.Sprintf("lock marker unreadable: %v", err)
}
