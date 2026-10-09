package gc

import (
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
)

// wirePorts plugs the driven adapters into the use case's ports: one
// store wearing all four hats (lock, recorder, identity, rows),
// the whole store behind them, and the registry API. Reporter,
// fence, probe, collect, and clock stay nil — the run renders
// and resolves all of them by default. Config rides no field:
// OpenDeps installed it as Current. Field-by-field, so a new
// gcrun.Deps need lands here or nowhere — never grown sideways
// at the call site.
func wirePorts(d *deps.Deps) gcrun.Deps {
	return gcrun.Deps{
		Lock: d.Store, Rec: d.Store, Ids: d.Store, Rows: d.Store,
		API: d.Reg, Store: d.Store,
	}
}
