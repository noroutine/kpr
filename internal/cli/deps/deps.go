package deps

import (
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Deps bundles one command's shared wiring: config, the redis state,
// and the registry client for the commands that need it (status,
// reap). Every RunE opens from this one place, so a wiring change
// (auth, dial opts, a second backend) lands everywhere or nowhere.
// Building the client eagerly is harmless: NewClient dials nothing,
// it only holds the base URL.
type Deps struct {
	Cfg   *config.Config
	Store store.StoreCloser
	Reg   *registry.Client
	// Backend and StoreDir name the resolved state backend:
	// resolved once here so commands never re-derive them (a
	// second resolution could answer differently).
	Backend  string
	StoreDir string
}

func OpenDeps() (*Deps, error) {
	cfg := config.NewBuilder().FromEnv().Build()
	// Installed, not just carried: use cases read config.Current()
	// where they need it, so the process reasons about this one
	// value — the same one every command wires from.
	config.SetCurrent(cfg)
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
	return &Deps{Cfg: cfg, Store: s, Reg: reg, Backend: backend, StoreDir: dir}, nil
}

func (d *Deps) Close() {
	_ = d.Store.Close()
}
