package storeops

import (
	"context"
	"errors"
	"io"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// lockFailsClosed is the store half broken: the marker never flips.
type lockFailsClosed struct {
	*store.MemStore
	err error
}

func (l lockFailsClosed) SetUnlocked(context.Context, bool) error { return l.err }

// A dead marker fails the lock instead of voicing it: Lock must
// not announce a transition it never made. If this fails, operators
// read "store locked" over a store that never locked.
func TestLockSurfacesMarkerFailure(t *testing.T) {
	want := errors.New("marker store down")
	s := lockFailsClosed{MemStore: store.NewMemStore(), err: want}
	if err := Lock(context.Background(), io.Discard, s); !errors.Is(err, want) {
		t.Errorf("Lock over dead marker = %v, want %v", err, want)
	}
}
