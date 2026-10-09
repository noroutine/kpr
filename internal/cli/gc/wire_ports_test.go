package gc

import (
	"testing"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/fence"
)

// wirePorts carries every gcrun need across, nothing dropped: one
// store in all four roles, the whole store behind them, and the
// registry API — reporter, fence, and clock stay nil for the run
// to default (probe/collect ride no field: package seams, W13).
// If this fails, a new Deps field is unplugged at the only call
// site.
func TestWirePortsCarriesWholeDeps(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	d, err := deps.OpenDeps()
	if err != nil {
		t.Fatalf("stage deps: %v", err)
	}
	defer d.Close()
	w := wirePorts(d)
	if w.Lock != d.Store || w.Rec != d.Store || w.Ids != d.Store || w.Rows != d.Store {
		t.Fatal("wirePorts splits the store across roles, want one object four times")
	}
	if w.API != d.Reg {
		t.Fatal("wirePorts drops the registry client")
	}
	if w.Store != fence.GateStore(d.Store) {
		t.Fatal("wirePorts drops the whole store: the fence resolves from its capability")
	}
	if w.Report != nil {
		t.Fatal("wirePorts sets the reporter: the run renders by default, only tests inject")
	}
	if w.Fence != nil {
		t.Fatal("wirePorts sets the fence: the run resolves it from the store, only tests inject")
	}
	if w.Clock != nil {
		t.Fatal("wirePorts sets the clock: production derives it from Current, only tests inject")
	}
}
