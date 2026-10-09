package gc

import (
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/fence"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
)

// wirePorts plugs the driven adapters into the use case's ports: one
// store wearing all four hats (lock, recorder, identity, rows), the
// registry API, plus the fence and production probe/collect seams.
// The reporter stays nil — the run renders by default. Config and
// the clock ride no field: OpenDeps installed the config as
// Current and a nil clock derives from it. Field-by-field, so a
// new gcrun.Deps need lands here or nowhere — never grown sideways
// at the call site.
func wirePorts(d *deps.Deps, f fence.Controller) gcrun.Deps {
	return gcrun.Deps{
		Lock: d.Store, Rec: d.Store, Ids: d.Store, Rows: d.Store,
		API: d.Reg, Fence: f,
		Probe: gcrun.ProbeRegistry, Collect: gcrun.RunCollector,
	}
}
