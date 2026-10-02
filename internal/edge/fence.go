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
	// StageHoldExpired fires when pushes flow past a present but
	// expired lease: gc outran its bound, and the hole must be
	// loud, never silent.
	StageHoldExpired = "hold_expired"
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

	mu          sync.Mutex
	lastDeny    bool
	lastHeld    bool
	lastExpired bool
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

// readLease parses the HOLD lease file: the expiry plus whether
// the file parses at all. Missing and corrupt leases read as
// absent (fail open — leases are not evidence); expiry is the
// caller's decision, so released (gone) and overrun (present
// but past) stay distinguishable.
func readLease(dir string) (until time.Time, present bool) {
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
	return lease.Until, true
}

// heldUntil returns the live lease expiry: only a present lease
// still in its term holds.
func heldUntil(dir string, now time.Time) (time.Time, bool) {
	until, present := readLease(dir)
	if !present || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
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
			g.flipExpired(false, "")
			if g.waitRelease(r, until) {
				g.flipExpired(true, "HOLD lease expired mid-collect: pushes flowing unfenced")
			}
		} else if _, present := readLease(g.Dir); present {
			// Present but past at arrival (heldUntil already said
			// no): a stale lease gc never released. Loud once,
			// then flow — same hole as mid-wait expiry.
			g.flipExpired(true, "HOLD lease expired mid-collect: pushes flowing unfenced")
		} else {
			g.flipExpired(false, "")
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

// waitRelease holds to the lease expiry, re-reading the file:
// an early release (file gone) wakes sleepers instead of serving
// the full nominal term. It reports whether the lease was still
// present-but-past at wake — gc outran its bound — so the caller
// can say the hole out loud. Client disconnect stops waiting: a
// gone client holds nothing.
func (g *Gate) waitRelease(r *http.Request, until time.Time) (expired bool) {
	for {
		// NOTE(mutants): <0 is equivalent — exact-zero expiry is
		// untestable clock granularity.
		if time.Until(until) <= 0 {
			until, present := readLease(g.Dir)
			return present && !until.After(g.now())
		}
		wait := time.Until(until)
		// NOTE(mutants): >= is equivalent — an exact-1s wait is
		// untestable timing; the cap only bounds the poll.
		if wait > time.Second {
			wait = time.Second
		}
		t := time.NewTimer(wait)
		select {
		case <-r.Context().Done():
			t.Stop()
			return false
		case <-t.C:
		}
		if _, held := heldUntil(g.Dir, g.now()); !held {
			until, present := readLease(g.Dir)
			return present && !until.After(g.now())
		}
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

func (g *Gate) flipExpired(expired bool, msg string) {
	g.mu.Lock()
	flipped := expired != g.lastExpired
	g.lastExpired = expired
	g.mu.Unlock()
	if !flipped {
		return
	}
	if expired {
		g.emit(StageHoldExpired, msg, "hold_expired")
	}
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
