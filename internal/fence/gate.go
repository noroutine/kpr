// Package fence is everything fencing: the ports (Controller,
// GateStore) plus the production behavior acting on them (Gate,
// Control). Ports and behavior share one address until a second
// disputed agreement forces a ports tree; until then the rule
// is small: the package narrates through the event vocabulary
// and imports no use case — drivers live above.
package fence

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Transition stages, beside the gc ones: same Event shape, same
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

// manifestRef matches reference-bearing registry paths: manifest
// PUT creates references, manifest DELETE destroys them. Blob
// uploads and every read pass the fence untouched.
var manifestRef = regexp.MustCompile(`^/v2/.+/manifests/.+$`)

// announce delivers one fence transition: a stage event plus a
// ring outcome, never per request. Nil report discards the
// event, nil store skips the ring.
func announce(st GateStore, report event.Reporter, now time.Time, stage, msg, outcome string) {
	event.Emit(report, event.Event{Stage: stage, Message: msg})
	if st == nil {
		return
	}
	_ = st.PushActivity(context.Background(), store.Outcome{
		Reason:  msg,
		Outcome: outcome,
		At:      now,
		Actor:   "kpr-edge",
	})
}

// Gate enforces HOLD/DENY around a proxied handler. DENY
// evaluates the lock through the same proof path the use cases
// mint from (token discarded — the fence needs a boolean, not
// a mint), so kpr's self-gating and the proxy's fencing cannot
// drift apart.
type Gate struct {
	Store GateStore
	// Lease is the HOLD source; nil reads absent everywhere
	// (marker-only fencing — DENY works over any backend).
	Lease Lease
	// Report receives flip events; nil discards.
	Report event.Reporter
	// Now sources time; nil means time.Now (tests pin it).
	Now func() time.Time

	mu          sync.Mutex
	lastDeny    bool
	lastExpired bool
	// lastHeld suppresses duplicate engage/release ring events
	// only — it is not state. Snapshot reads the lease file;
	// wiring this back into it reintroduces the stale card.
	lastHeld bool
}

// lease opens the HOLD source: the backend owns the lease it
// already hosts, the gate only reads it. A nil lease reads
// absent — marker-only fencing.
func (g *Gate) lease() Lease {
	if g.Lease == nil {
		return nilLease{}
	}
	return g.Lease
}

// nilLease reads absent everywhere: the marker-only gate's
// stand-in, so callers never branch on the lease.
type nilLease struct{}

func (nilLease) Hold(context.Context, time.Time) (func(), error) {
	return nil, errors.New("hold without a lease")
}

func (nilLease) Read() (time.Time, bool) { return time.Time{}, false }

func (nilLease) HeldUntil(time.Time) (time.Time, bool) { return time.Time{}, false }

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
		// One read, evaluated locally: a second read could see a
		// fresh lease the first missed and misreport it as stale.
		if until, present := g.lease().Read(); present && until.After(now) {
			g.flipHeld(true, "gc finalize holds manifest writes")
			g.flipExpired(false, "")
			if g.waitRelease(r, until) {
				g.flipExpired(true, "HOLD lease expired mid-collect: pushes flowing unfenced")
			}
		} else if present {
			// Present but past at arrival: a stale lease gc never
			// released. Loud once, then flow — same hole as
			// mid-wait expiry.
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

// waitRelease holds to the lease expiry, re-reading the lease:
// an early release (lease gone) wakes sleepers instead of serving
// the full nominal term. It reports whether the lease was still
// present-but-past at wake — gc outran its bound — so the caller
// can say the hole out loud. Client disconnect stops waiting: a
// gone client holds nothing.
func (g *Gate) waitRelease(r *http.Request, until time.Time) (expired bool) {
	for {
		// NOTE(mutants): <0 is equivalent — exact-zero expiry is
		// untestable clock granularity.
		if time.Until(until) <= 0 {
			until, present := g.lease().Read()
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
		if _, held := g.lease().HeldUntil(g.now()); !held {
			until, present := g.lease().Read()
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

// Snapshot reports the fence's current posture for the management
// console: whether mutating traffic is denied (locked store) and
// whether a HOLD lease is pinning it. Deny is the last evaluation
// (the gate learns locks from traffic); held reads the live lease,
// never the last flip — a quiet edge must still show a hold gc
// engaged, and a released hold must read free with no traffic
// after. The console shows what IS, the ring shows what CHANGED.
func (g *Gate) Snapshot() (deny, held bool) {
	g.mu.Lock()
	deny = g.lastDeny
	g.mu.Unlock()
	_, held = g.lease().HeldUntil(g.now())
	return deny, held
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
	announce(g.Store, g.Report, g.now(), stage, msg, outcome)
}

func describeLockErr(err error) string {
	if errors.Is(err, proof.ErrLocked) {
		return "store locked: manifest writes denied"
	}
	return fmt.Sprintf("lock marker unreadable: %v", err)
}
