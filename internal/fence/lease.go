package fence

import (
	"context"
	"fmt"
	"io"
	"time"

	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Lease is the HOLD lease port both media implement: engage with
// an expiry, read back the term, evaluate it live. Reads never
// fail — anything unreadable is absent (fail open, leases are
// not evidence) — while a Hold that fails to engage errors, so
// the caller refuses instead of collecting unfenced.
type Lease interface {
	Hold(ctx context.Context, until time.Time) (release func(), err error)
	Read() (until time.Time, present bool)
	HeldUntil(now time.Time) (until time.Time, held bool)
}

// fileLeaseStore is the lease-hosting capability both processes
// see through the file backend: the store's own dir.
type fileLeaseStore interface {
	HoldDir() (string, bool)
}

// redisLeaseStore is the same capability through the shared
// redis: a conn on the store's client and DB, no second dial.
type redisLeaseStore interface {
	HoldLeaseConn() (store.LeaseConn, bool)
}

// LeaseForStore resolves the lease the store advertises: file
// dir first, shared redis second, nil (marker-only fencing)
// otherwise. No backend names travel here — capability decides.
// A nil lease reads absent everywhere, so callers never branch.
func LeaseForStore(st GateStore) Lease {
	if hs, ok := st.(fileLeaseStore); ok {
		if dir, ok := hs.HoldDir(); ok {
			return store.FileLease{Dir: dir}
		}
	}
	if hs, ok := st.(redisLeaseStore); ok {
		if conn, ok := hs.HoldLeaseConn(); ok {
			return store.RedisLease{Conn: conn, Key: store.HoldLeaseKey}
		}
	}
	return nil
}

// ControllerForStore decides the run's fence AND builds it from
// the store's own capability (the leaseReady the preflight takes
// is simply this returning non-nil). Previews stay silent either
// way — nothing is deleted, so nothing holds; armed runs warn
// out loud before collecting unfenced. Armed travels as the
// mint, never a bool: the signature is the point of proofs.
func ControllerForStore(st GateStore, armed proof.ArmedRun, out io.Writer) Controller {
	if l := LeaseForStore(st); l != nil {
		return Control{Lease: l, Store: st}
	}
	if !proof.Unarmed(armed) {
		_, _ = fmt.Fprintln(out, "Warning: proxy HOLD fence unavailable without a shared store — armed collect runs unfenced")
	}
	return nil
}
