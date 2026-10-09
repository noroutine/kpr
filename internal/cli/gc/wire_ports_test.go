package gc

import (
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// wirePorts carries every gcrun need across, nothing dropped: one
// store in all four roles plus the registry API and the fence
// alongside, no reporter and no clock — the run renders by
// default and production derives the clock from Current. If this
// fails, a new Deps field is unplugged at the only call site.
func TestWirePortsCarriesWholeDeps(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	d, err := deps.OpenDeps()
	if err != nil {
		t.Fatalf("stage deps: %v", err)
	}
	defer d.Close()
	var out strings.Builder
	w := wirePorts(d, proof.Arm(true, false), &out)
	if w.Lock != d.Store || w.Rec != d.Store || w.Ids != d.Store || w.Rows != d.Store {
		t.Fatal("wirePorts splits the store across roles, want one object four times")
	}
	if w.API != d.Reg {
		t.Fatal("wirePorts drops the registry client")
	}
	if w.Report != nil {
		t.Fatal("wirePorts sets the reporter: the run renders by default, only tests inject")
	}
	if _, ok := w.Fence.(fence.Control); !ok {
		t.Fatalf("wirePorts fence = %T, want the file backend's Control", w.Fence)
	}
	if out.Len() != 0 {
		t.Fatalf("wirePorts warned %q on a shared file store, want silence", out.String())
	}
	if w.Probe == nil || w.Collect == nil {
		t.Fatal("wirePorts leaves the seams unset: production runs the stock probe and collector")
	}
	if w.Clock != nil {
		t.Fatal("wirePorts sets the clock: production derives it from Current, only tests inject")
	}
}
