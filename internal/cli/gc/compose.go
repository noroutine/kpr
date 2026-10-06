package gc

import (
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/fence"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
)

// composeDeps is the command's composition root in miniature: it plugs
// the driven adapters (one store wearing all four hats, the registry
// API) plus the run-assembled reporter, fence, and production
// probe/collect seams into the use case's ports. Config and the clock
// ride no field: OpenDeps installed the config as Current and a nil
// clock derives from it. Field-by-field, so a new gcrun.Deps need lands
// here or nowhere — never grown sideways at the call site.
func composeDeps(d *deps.Deps, report gcrun.Reporter, f fence.Controller) gcrun.Deps {
	return gcrun.Deps{
		Lock: d.Store, Rec: d.Store, Ids: d.Store, Rows: d.Store,
		API: d.Reg, Report: report, Fence: f,
		Probe: gcrun.ProbeRegistry, Collect: gcrun.RunCollector,
	}
}
