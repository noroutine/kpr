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
}

func OpenDeps() (*Deps, error) {
	cfg := config.NewBuilder().FromEnv().Build()
	// Installed, not just carried: use cases read config.Current()
	// where they need it, so the process reasons about this one
	// value — the same one every command wires from.
	config.SetCurrent(cfg)
	s, err := OpenStore(cfg)
	if err != nil {
		return nil, err
	}
	reg := registry.NewClient(cfg.RegistryURL)
	reg.SetBasicAuth(cfg.RegistryUser, cfg.RegistryPassword)
	return &Deps{Cfg: cfg, Store: s, Reg: reg}, nil
}

func (d *Deps) Close() {
	_ = d.Store.Close()
}
