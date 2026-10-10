package proof

import (
	"context"
	"errors"
)

// ErrLocked refuses a locked store. The message stays identical
// to the runtime refusal it replaces — only the place that
// guarantees it moves from runtime to signature.
var ErrLocked = errors.New("store is locked: registry-store writes are denied — run `kpr store unlock` to prove the shared store and allow them")

// UnlockedStore is operator intent to allow registry-store
// writes: `kpr store unlock` proved the shared store and opened
// it. Sealed like every evidence: the only inhabitant comes from
// ProveUnlockedStore, and the zero value is nil.
type UnlockedStore interface {
	sealed()
}

type unlockedStore struct{}

func (unlockedStore) sealed() {}

// Locker reports the marker; store.Store satisfies it
// structurally, so the use case names only what it reads.
type Locker interface {
	IsUnlocked(ctx context.Context) (bool, error)
}

// ProveUnlockedStore reads the marker once: unreadable
// propagates, absent refuses with ErrLocked, present produces.
func ProveUnlockedStore(ctx context.Context, l Locker) (UnlockedStore, error) {
	ok, err := l.IsUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLocked
	}
	return unlockedStore{}, nil
}
