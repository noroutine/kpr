package cli

import (
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// deps bundles one command's shared wiring: config, the redis state,
// and the registry client for the commands that need it (status,
// reap). Every RunE opens from this one place, so a wiring change
// (auth, dial opts, a second backend) lands everywhere or nowhere.
// Building the client eagerly is harmless: NewClient dials nothing,
// it only holds the base URL.
type deps struct {
	cfg   *config.Config
	store store.StoreCloser
	reg   *registry.Client
}

func openDeps() (*deps, error) {
	cfg := config.NewBuilder().FromEnv().Build()
	s, err := OpenStore(cfg)
	if err != nil {
		return nil, err
	}
	return &deps{cfg: cfg, store: s, reg: registry.NewClient(cfg.RegistryURL)}, nil
}

func (d *deps) close() {
	_ = d.store.Close()
}
