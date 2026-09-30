// Package store is kpr state behind one port: rows, marks, run
// state, and the single-flight locks (see docs/STORES.md for the
// redis, mem, and file shapes). Backends hold state, never log
// streams: the activity ring is capped, and verbose operational logs
// stay on stdout/OTLP.
package store

import (
	"context"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
)

// Redis keys. Rows carry no redis key TTL of their own: the sweeper
// must see an expired row to delete it, and removes the row only after
// the registry confirms.
const (
	// RowsKey is a HASH of repo\x00tag -> row JSON.
	RowsKey = "kpr:rows"
	// CurrentKey is one JSON record, overwritten through a pass.
	CurrentKey = "kpr:sweep:current"
	// ActivityKey is the capped outcome ring (newest first).
	ActivityKey = "kpr:sweep:activity"
	// LockKey is the sweeper single-flight lock (expires).
	LockKey = "kpr:sweep:lock"
	// GCLockKey serializes collectors: kpr gc and make gc honor the
	// same key, so the two never race the store. Manual collector
	// runs bypass it — the registry itself sets no lock.
	GCLockKey = "kpr:gc:lock"
	// UnlockedKey records operator intent to allow registry-store
	// writes: present means `kpr unlock` proved the shared store
	// and opened it. Absent (fresh stores included) reads locked —
	// default-deny, never default-allow.
	UnlockedKey = "kpr:store:unlocked"
)

// ActivityCap bounds the outcome ring: state, not a stream.
const ActivityCap = 100

// Current is the one pass record overwritten through a sweep: what the
// sweeper is doing right now, without anyone streaming logs.
type Current struct {
	PassID    string    `json:"pass_id"`
	Stage     string    `json:"stage"`
	Trigger   string    `json:"trigger"`
	StartedAt time.Time `json:"started_at"`
	Due       int       `json:"due"`
	Done      int       `json:"done"`
}

// Outcome is one attempted row: small JSON for the activity ring.
type Outcome struct {
	Repo    string    `json:"repo"`
	Tag     string    `json:"tag"`
	Reason  string    `json:"reason"`
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
}

// Store is the shared state both `serve` and the colocated CLI talk
// to. Implementations: MemStore (hermetic tests), RedisStore
// (default), and FileStore (redis-less mode). Shapes documented in
// docs/STORES.md; semantics pinned by the storetest contract.

// StoreCloser is a Store with lifecycle: both production backends.
// MemStore stays Close-less (tests own it outright).
type StoreCloser interface {
	Store
	Close() error
}
type Store interface {
	// Ping reports backend reachability (banner red, sweeper skips).
	Ping(ctx context.Context) error
	// Record upserts a tracked row. A newer push time restarts the
	// minimal promise and clears a stale due mark; a re-notification
	// of the same push preserves it.
	Record(ctx context.Context, r policy.Row) error
	// All returns every tracked row.
	All(ctx context.Context) ([]policy.Row, error)
	// Due returns only rows marked due.
	Due(ctx context.Context) ([]policy.Row, error)
	// MarkDue marks a row due with a sweep reason (the reap interface).
	MarkDue(ctx context.Context, repo, tag, reason string) error
	// ClearDue drops every due mark, returning how many went (the
	// plan-discard interface). Rows survive; only marks go.
	ClearDue(ctx context.Context) (int, error)
	// UnmarkDue drops one row's due mark, reporting whether a mark
	// was held (the plan-remove interface). Rows survive.
	UnmarkDue(ctx context.Context, repo, tag string) (bool, error)
	// Delete removes a row after the registry confirms the delete.
	Delete(ctx context.Context, repo, tag string) error
	// SetCurrent/GetCurrent overwrite/read the one pass record.
	SetCurrent(ctx context.Context, c Current) error
	GetCurrent(ctx context.Context) (Current, error)
	// PushActivity prepends an outcome, trimming the ring to the cap.
	PushActivity(ctx context.Context, o Outcome) error
	// Activity reads the ring, newest first.
	Activity(ctx context.Context) ([]Outcome, error)
	// AcquireLock takes one named single-flight lock (false = held:
	// the sweep pass skips, the collector run refuses). Name selects
	// the lock: LockKey for sweeps, GCLockKey for collectors.
	AcquireLock(ctx context.Context, name string, ttl time.Duration) (bool, error)
	// ReleaseLock drops the named lock after the run.
	ReleaseLock(ctx context.Context, name string) error
	// IsUnlocked reports operator intent for registry-store writes:
	// false (including fresh stores, where nothing was ever set)
	// means locked — gc and future writers refuse before proving
	// anything. A read failure is an error, never a guess.
	IsUnlocked(ctx context.Context) (bool, error)
	// SetUnlocked records or clears the intent: true after `kpr
	// unlock` proves the shared store, false on `kpr lock`.
	SetUnlocked(ctx context.Context, unlocked bool) error
}
