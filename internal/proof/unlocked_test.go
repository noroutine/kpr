package proof

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// stubLocker is a marker with a script: open, shut, or
// unreadable. If proving intent needs the store, the seam is
// wrong.
type stubLocker struct {
	open bool
	err  error
}

func (s stubLocker) IsUnlocked(context.Context) (bool, error) {
	return s.open, s.err
}

// An open marker produces: unlock proved the store, intent stands.
// If this fails, proven intent stopped opening.
func TestProveUnlockedStoreProducesWhenOpen(t *testing.T) {
	u, err := ProveUnlockedStore(context.Background(), stubLocker{open: true})
	if err != nil {
		t.Fatalf("open marker = %v, want the proof", err)
	}
	if u == nil {
		t.Fatal("open marker produced nil, want UnlockedStore")
	}
}

// A shut marker refuses with the identical message — only the
// place guaranteeing it moved. If this fails, locked stores
// stopped refusing, or the refusal drifted.
func TestProveUnlockedStoreRefusesWhenShut(t *testing.T) {
	u, err := ProveUnlockedStore(context.Background(), stubLocker{})
	if u != nil {
		t.Errorf("shut marker produced %v, want nothing", u)
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("shut marker error = %v, want ErrLocked", err)
	}
}

// An unreadable marker propagates: unknown is not intent. If
// this fails, outages started producing.
func TestProveUnlockedStorePropagatesUnreadable(t *testing.T) {
	boom := errors.New("boom")
	u, err := ProveUnlockedStore(context.Background(), stubLocker{err: boom})
	if u != nil {
		t.Errorf("unreadable marker produced %v, want nothing", u)
	}
	if !errors.Is(err, boom) {
		t.Errorf("unreadable marker error = %v, want the store error", err)
	}
}

// The zero value is nothing: without the marker there is no
// intent. If this fails, intent can be produced from thin air.
func TestUnlockedStoreZeroIsNothing(t *testing.T) {
	var u UnlockedStore
	if u != nil {
		t.Errorf("zero UnlockedStore = %v, want nil", u)
	}
}

// The seal moves the guarantee, never the words: gc returns this
// exact value (errors.Is), so the operator-facing refusal has one
// source of truth. If this fails, someone reworded a refusal
// operators match on — say why, loudly, in the commit.
func TestErrLockedKeepsItsWords(t *testing.T) {
	const want = "store is locked: registry-store writes are denied — run `kpr store unlock` to prove the shared store and allow them"
	if ErrLocked.Error() != want {
		t.Errorf("ErrLocked = %q, want %q", ErrLocked.Error(), want)
	}
}

// A stage takes its evidence as an argument: without
// UnlockedStore there is no call. If this fails, intent stopped
// meaning it.
func ExampleProveUnlockedStore() {
	u, _ := ProveUnlockedStore(context.Background(), stubLocker{open: true})
	writeIfUnlockedStore(u)
	// Output: writing to shared store
}

func writeIfUnlockedStore(UnlockedStore) {
	fmt.Println("writing to shared store")
}
