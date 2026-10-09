package gc

import (
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// The file backend builds a Control over the shared store dir,
// silently: the edge reads what gc writes. If this fails, the
// lease stopped reaching the edge.
func TestFenceForBackendWiresFileStore(t *testing.T) {
	var out strings.Builder
	dir := t.TempDir()
	fenced := FenceForBackend("file", dir, store.NewMemStore(), flagArmed(), &out)
	ctl, ok := fenced.(fence.Control)
	if !ok {
		t.Fatalf("file backend fence = %T, want a fence.Control", fenced)
	}
	if ctl.Dir != dir {
		t.Errorf("lease dir = %q, want the shared store dir", ctl.Dir)
	}
	if out.Len() != 0 {
		t.Errorf("wired fence warned %q, want silence", out.String())
	}
}

// Anything else runs unfenced with the warning said out loud on
// armed runs — and silent on previews, where nothing holds. If
// this fails, unfenced collects went quiet or previews warned
// for no reason.
func TestFenceForBackendWarnsWhenUnshared(t *testing.T) {
	st := store.NewMemStore()
	var armed strings.Builder
	if fenced := FenceForBackend("redis", "", st, flagArmed(), &armed); fenced != nil {
		t.Errorf("redis backend fence = %v, want nil", fenced)
	}
	if !strings.Contains(armed.String(), "unfenced") {
		t.Errorf("armed unfenced warning = %q, want it said", armed.String())
	}

	var preview strings.Builder
	if fenced := FenceForBackend("redis", "", st, nil, &preview); fenced != nil {
		t.Errorf("preview fence = %v, want nil", fenced)
	}
	if preview.Len() != 0 {
		t.Errorf("preview warned %q, want silence", preview.String())
	}

	var broken strings.Builder
	if fenced := FenceForBackend("", "", st, flagArmed(), &broken); fenced != nil {
		t.Errorf("broken backend fence = %v, want nil", fenced)
	}
	if !strings.Contains(broken.String(), "unfenced") {
		t.Errorf("broken backend warning = %q, want it said", broken.String())
	}
}
