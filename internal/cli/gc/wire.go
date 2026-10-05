package gc

import (
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
)

// wire maps the shared command wiring onto what a gc run needs:
// one store wearing all four hats, the registry API, the clock
// its mint checks. Config rides no field: OpenDeps installed it
// as Current. Field-by-field, so a new gcrun.Deps need lands
// here or nowhere — never grown sideways at the call site.
func wire(d *deps.Deps) gcrun.Deps {
	return gcrun.Deps{
		Lock: d.Store, Rec: d.Store, Ids: d.Store, Rows: d.Store,
		API: d.Reg, Clock: deps.ClockSource(d.Cfg),
	}
}
