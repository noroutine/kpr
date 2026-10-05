package gc

import (
	"io"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
)

// wire carries every gcrun need across, nothing dropped: one
// store in all four roles plus the registry API, the assembled
// reporter and fence alongside, and no clock — production
// derives it from Current. If this fails, a new Deps field is
// unwired at the only call site.
func TestWireCarriesWholeDeps(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	d, err := deps.OpenDeps()
	if err != nil {
		t.Fatalf("stage deps: %v", err)
	}
	defer d.Close()
	report := renderGCEvent(io.Discard, true)
	w := wire(d, report, nil)
	if w.Lock != d.Store || w.Rec != d.Store || w.Ids != d.Store || w.Rows != d.Store {
		t.Fatal("wire split the store across roles, want one object four times")
	}
	if w.API != d.Reg {
		t.Fatal("wire dropped the registry client")
	}
	if w.Report == nil {
		t.Fatal("wire dropped the reporter")
	}
	if w.Fence != nil {
		t.Fatal("wire invented a fence: the command assembles it per backend")
	}
	if w.Probe == nil || w.Collect == nil {
		t.Fatal("wire left the seams unset: production runs the stock probe and collector")
	}
	if w.Clock != nil {
		t.Fatal("wire set the clock: production derives it from Current, only tests inject")
	}
}
