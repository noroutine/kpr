package gc

import (
	"io"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// wirePorts plugs the driven adapters into the use case's ports: one
// store wearing all four hats (lock, recorder, identity, rows), the
// registry API, and production probe/collect seams — plus the fence
// resolved from the deps' own backend (file shares the lease dir,
// anything else runs unfenced with the warning said out loud).
// The reporter stays nil — the run renders by default. Config and
// the clock ride no field: OpenDeps installed the config as
// Current and a nil clock derives from it. Field-by-field, so a
// new gcrun.Deps need lands here or nowhere — never grown sideways
// at the call site.
func wirePorts(d *deps.Deps, armed proof.ArmedRun, out io.Writer) gcrun.Deps {
	return gcrun.Deps{
		Lock: d.Store, Rec: d.Store, Ids: d.Store, Rows: d.Store,
		API: d.Reg, Fence: gcrun.FenceForBackend(d.Backend, d.StoreDir, d.Store, armed, out),
		Probe: gcrun.ProbeRegistry, Collect: gcrun.RunCollector,
	}
}
