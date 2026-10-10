package proof

// SameStore is proof of same-store identity (this mount is the
// paired store — not liveness, which only a mint proves). Sealed:
// the only inhabitants come from Prover.Prove, which runs the
// read gate and refuses anything but proceed. Stale rides along
// as a flag — callers apply their own policy.

import (
	"context"
	"fmt"
	"time"

	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Store is what proving reads: the pairing plus every tracked
// row. The full store satisfies it; the prover declares only this.
type Store interface {
	All(ctx context.Context) ([]policy.Row, error)
	GetIdentity(ctx context.Context) (store.Identity, error)
}

// SameStore is proof that the mount is the paired store. It is a
// sealed interface — the unexported method means no outside
// package can implement it, so the only inhabitants are the ones
// Prove returns, and the zero value is just nil. Consumers check
// nil, never fields: possession of a non-nil SameStore IS the
// proof, and there is no empty value to sneak through a signature.
type SameStore interface {
	// Generation is the served generation the proof was read from.
	Generation() string
	// Identity is the paired lineage the proof was judged against.
	Identity() string
	// Stale reports the served generation lagging the tracked state:
	// identity holds, currency does not.
	Stale() bool

	sealed()
}

// sameStore is the sole implementation. Unconstructible outside
// this package by construction, not by convention.
type sameStore struct {
	gen   string
	id    string
	stale bool
}

func (p sameStore) Generation() string { return p.gen }
func (p sameStore) Identity() string   { return p.id }
func (p sameStore) Stale() bool        { return p.stale }
func (p sameStore) sealed()            {}

// Prover reads same-store proof. Sentinel is the served side, Store
// the paired side; Armed decides whether the read may establish
// (refuse unarmed — nothing may establish from a read-only path).
// Zero is preview: producers light Armed loud, never by default.
type Prover struct {
	Sentinel sentinel.API
	Store    Store
	Armed    bool
	// Now is a seam for tests; production leaves it nil (wall clock).
	Now func() time.Time
}

func (p Prover) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Prove runs the read gate and returns the proof, or refuses with
// the remedy named. Anything but proceed — foreign, unpaired,
// unreadable, or establishing silence — is an error, never a
// guess: the caller wanted evidence and there is none.
func (p Prover) Prove(ctx context.Context) (SameStore, error) {
	if p.Sentinel == nil {
		return nil, fmt.Errorf("prover miswired: no sentinel reader (refusing instead of proving blind)")
	}
	pay, digest, rerr := sentinel.Read(ctx, p.Sentinel, sentinel.Repo, sentinel.Tag)
	ident, err := p.Store.GetIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("lineage unreadable: %v", err)
	}
	rows, err := p.Store.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("tracked state unreadable: %v", err)
	}
	v := lineage.Judge(
		lineage.Served{Payload: pay, Digest: digest, Err: rerr},
		lineage.Local{Ident: ident, Rows: rows},
		lineage.Ask{Armed: p.Armed, Now: p.now()})
	if !v.Proceed || v.Establish {
		return nil, fmt.Errorf("%s — %s", v.Reason, v.Action)
	}
	return sameStore{gen: pay.Gen, id: ident.ID, stale: v.Stale}, nil
}
