package fence

import (
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// A lease-hosting store builds a Control over its own dir,
// silently: the edge reads what gc writes. If this fails, the
// lease stopped reaching the edge.
func TestControllerForStoreWiresFileStore(t *testing.T) {
	var out strings.Builder
	dir := t.TempDir()
	fenced := ControllerForStore(store.NewFileStore(dir), proof.Arm(true, false), &out)
	ctl, ok := fenced.(Control)
	if !ok {
		t.Fatalf("file backend fence = %T, want a Control", fenced)
	}
	fl, ok := ctl.Lease.(store.FileLease)
	if !ok {
		t.Fatalf("file backend lease = %T, want a store.FileLease", ctl.Lease)
	}
	if fl.Dir != dir {
		t.Errorf("lease dir = %q, want the store's own dir", fl.Dir)
	}
	if out.Len() != 0 {
		t.Errorf("wired fence warned %q, want silence", out.String())
	}
}

// A store without the lease capability runs unfenced with the
// warning said out loud on armed runs — and silent on previews,
// where nothing holds. If this fails, unfenced collects went
// quiet or previews warned for no reason.
func TestControllerForStoreWarnsWhenUnshared(t *testing.T) {
	st := store.NewMemStore()
	var armed strings.Builder
	if fenced := ControllerForStore(st, proof.Arm(true, false), &armed); fenced != nil {
		t.Errorf("capability-less fence = %v, want nil", fenced)
	}
	if !strings.Contains(armed.String(), "unfenced") {
		t.Errorf("armed unfenced warning = %q, want it said", armed.String())
	}

	var preview strings.Builder
	if fenced := ControllerForStore(st, nil, &preview); fenced != nil {
		t.Errorf("preview fence = %v, want nil", fenced)
	}
	if preview.Len() != 0 {
		t.Errorf("preview warned %q, want silence", preview.String())
	}
}
