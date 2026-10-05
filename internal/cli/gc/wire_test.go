package gc

import (
	"testing"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
)

// wire carries every gcrun need across, nothing dropped: one
// store in all four roles, the registry API with its strings,
// the configured clock. If this fails, a new Deps field is
// unwired at the only call site.
func TestWireCarriesWholeDeps(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	d, err := deps.OpenDeps()
	if err != nil {
		t.Fatalf("stage deps: %v", err)
	}
	defer d.Close()
	w := wire(d)
	if w.Lock != d.Store || w.Rec != d.Store || w.Ids != d.Store || w.Rows != d.Store {
		t.Fatal("wire split the store across roles, want one object four times")
	}
	if w.API != d.Reg {
		t.Fatal("wire dropped the registry client")
	}
	if w.Clock == nil {
		t.Fatal("wire dropped the clock")
	}
}
