// Package trust names the store's lineage in one word off live
// reads: paired when the dry verdict holds, a mistrust name per
// shape when it doesn't. A read-only use case over injected ports
// — analyze's parenthesis and the status headline share it, so
// both judge by the same word. Best-effort throughout: every
// failure degrades to a name, never an error, never a refusal,
// never a mint.
package trust

import (
	"context"
	"errors"
	"time"

	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// errNoProofReader stands in when no proof port exists (nil API):
// indistinguishable from evidence trouble, never from silence.
var errNoProofReader = errors.New("no proof reader")

// Word runs the dry lineage verdict over live reads and names it:
// paired when it holds, unpaired (fresh/wiped), unserved (paired
// store, silent registry), unproven (evidence unusable), foreign,
// rollback?, unadopted (store behind the serving registry). The
// dry verdict never mints or heals here.
func Word(ctx context.Context, api sentinel.API, ident store.Identity, rows []policy.Row) string {
	served := lineage.Served{Err: errNoProofReader}
	if api != nil {
		pay, rerr := sentinel.LastProof(ctx, api)
		served = lineage.Served{Payload: pay, Err: rerr}
	}
	v := lineage.Judge(served, lineage.Local{Ident: ident, Rows: rows},
		lineage.Ask{DryRun: true, Now: time.Now().UTC()})
	return name(served, ident, rows, v)
}

// name maps one judged case to its display word. The cheap shapes
// read straight off observables (pairing, served identity, served
// silence); the temporal one (served older than tracked newest)
// arrives as the caller's dry Judge verdict.
func name(s lineage.Served, ident store.Identity, rows []policy.Row, v lineage.Verdict) string {
	if ident.ID == "" {
		return "unpaired"
	}
	if s.Err != nil {
		if sentinel.Absent(s.Err) {
			return "unserved"
		}
		return "unproven"
	}
	if s.Payload.ID != ident.ID {
		return "foreign"
	}
	if v.Stale {
		return "rollback?"
	}
	if !v.Proceed {
		return "unproven"
	}
	for _, r := range rows {
		if r.Repo == sentinel.Repo && r.Tag == s.Payload.Gen {
			return "paired"
		}
	}
	return "unadopted"
}
