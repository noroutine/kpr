package gc

import (
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
)

// wire maps the shared command wiring onto what a gc run needs:
// one store wearing all four hats plus the registry API. Config
// and the clock ride no field: OpenDeps installed the config as
// Current and a nil clock derives from it. Field-by-field, so a
// new gcrun.Deps need lands here or nowhere — never grown
// sideways at the call site.
func wire(d *deps.Deps) gcrun.Deps {
	return gcrun.Deps{
		Lock: d.Store, Rec: d.Store, Ids: d.Store, Rows: d.Store,
		API: d.Reg,
	}
}
