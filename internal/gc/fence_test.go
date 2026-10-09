package gc

import (
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// A lease-hosting store builds a Control over its own dir,
// silently: the edge reads what gc writes. If this fails, the
// lease stopped reaching the edge.
func TestFenceForBackendWiresFileStore(t *testing.T) {
	var out strings.Builder
	dir := t.TempDir()
	fenced := fenceForBackend(store.NewFileStore(dir), flagArmed(), &out)
	ctl, ok := fenced.(fence.Control)
	if !ok {
		t.Fatalf("file backend fence = %T, want a fence.Control", fenced)
	}
	if ctl.Dir != dir {
		t.Errorf("lease dir = %q, want the store's own dir", ctl.Dir)
	}
	if out.Len() != 0 {
		t.Errorf("wired fence warned %q, want silence", out.String())
	}
}

// A store without the lease capability runs unfenced with the
// warning said out loud on armed runs — and silent on previews,
// where nothing holds. If this fails, unfenced collects went
// quiet or previews warned for no reason.
func TestFenceForBackendWarnsWhenUnshared(t *testing.T) {
	st := store.NewMemStore()
	var armed strings.Builder
	if fenced := fenceForBackend(st, flagArmed(), &armed); fenced != nil {
		t.Errorf("capability-less fence = %v, want nil", fenced)
	}
	if !strings.Contains(armed.String(), "unfenced") {
		t.Errorf("armed unfenced warning = %q, want it said", armed.String())
	}

	var preview strings.Builder
	if fenced := fenceForBackend(st, nil, &preview); fenced != nil {
		t.Errorf("preview fence = %v, want nil", fenced)
	}
	if preview.Len() != 0 {
		t.Errorf("preview warned %q, want silence", preview.String())
	}
}
