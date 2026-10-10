package deps

import (
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Deps bundles one command's shared wiring: the state store and
// the registry client. Every RunE opens from this one place, so a
// wiring change (auth, dial opts, a second backend) lands
// everywhere or nowhere. Config rides no field: OpenDeps builds
// it from env, installs it as Current, and wires store and client
// from it — use cases read config.Current() where they need it.
// Building the client eagerly is harmless: NewClient dials
// nothing, it only holds the base URL.
type Deps struct {
	Store store.StoreCloser
	Reg   *registry.Client
}

func OpenDeps() (*Deps, error) {
	cfg := config.NewBuilder().FromEnv().Build()
	// Installed, not just carried: use cases read config.Current()
	// where they need it, so the process reasons about this one
	// value — the same one every command wires from.
	config.SetCurrent(cfg)
	// Resolved once here, never re-derived downstream: a second
	// resolution could answer differently.
	backend, dir, err := ResolveStoreBackend()
	if err != nil {
		return nil, err
	}
	s, err := OpenStore(cfg, backend, dir)
	if err != nil {
		return nil, err
	}
	reg := registry.NewClient(cfg.RegistryURL)
	reg.SetBasicAuth(cfg.RegistryUser, cfg.RegistryPassword)
	return &Deps{Store: s, Reg: reg}, nil
}

func (d *Deps) Close() {
	_ = d.Store.Close()
}
