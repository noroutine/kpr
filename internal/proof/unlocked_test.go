package proof

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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

// An open marker mints: unlock proved the store, intent stands.
// If this fails, proven intent stopped opening.
func TestProveUnlockedStoreMintsWhenOpen(t *testing.T) {
	u, err := ProveUnlockedStore(context.Background(), stubLocker{open: true})
	if err != nil {
		t.Fatalf("open marker = %v, want mint", err)
	}
	if u == nil {
		t.Fatal("open marker minted nil, want UnlockedStore")
	}
}

// A shut marker refuses with the identical message — only the
// place guaranteeing it moved. If this fails, locked stores
// stopped refusing, or the refusal drifted.
func TestProveUnlockedStoreRefusesWhenShut(t *testing.T) {
	u, err := ProveUnlockedStore(context.Background(), stubLocker{})
	if u != nil {
		t.Errorf("shut marker minted %v, want nothing", u)
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("shut marker error = %v, want ErrLocked", err)
	}
}

// An unreadable marker propagates: unknown is not intent. If
// this fails, outages started minting.
func TestProveUnlockedStorePropagatesUnreadable(t *testing.T) {
	boom := errors.New("boom")
	u, err := ProveUnlockedStore(context.Background(), stubLocker{err: boom})
	if u != nil {
		t.Errorf("unreadable marker minted %v, want nothing", u)
	}
	if !errors.Is(err, boom) {
		t.Errorf("unreadable marker error = %v, want the store error", err)
	}
}

// The zero value is nothing: without the marker there is no
// intent. If this fails, intent can be minted from thin air.
func TestUnlockedStoreZeroIsNothing(t *testing.T) {
	var u UnlockedStore
	if u != nil {
		t.Errorf("zero UnlockedStore = %v, want nil", u)
	}
}

// The seal moves the guarantee, never the words: ErrLocked reads
// exactly like the runtime refusal in gc/run.go. If this fails,
// the refusal drifted between the two places.
func TestErrLockedMatchesGcRefusal(t *testing.T) {
	raw, err := os.ReadFile("../gc/run.go")
	if err != nil {
		t.Skip("gc source unreadable")
	}
	if !strings.Contains(string(raw), ErrLocked.Error()) {
		t.Errorf("ErrLocked %q not found verbatim in gc/run.go", ErrLocked.Error())
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
