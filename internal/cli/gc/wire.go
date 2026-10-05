package gc

import (
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
)

// wire maps the shared command wiring onto what a gc run needs:
// one store wearing all four hats, the registry API with the
// strings naming it, the clock its mint checks. Field-by-field,
// so a new gcrun.Deps need lands here or nowhere — never grown
// sideways at the call site.
func wire(d *deps.Deps) gcrun.Deps {
	return gcrun.Deps{
		Lock: d.Store, Rec: d.Store, Ids: d.Store, Rows: d.Store,
		API:         d.Reg,
		RegistryURL: d.Cfg.RegistryURL, ConfigPath: d.Cfg.RegistryConfig,
		TimeServer: d.Cfg.TimeServer, Clock: deps.ClockSource(d.Cfg),
	}
}
