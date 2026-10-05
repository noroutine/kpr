package deps

import (
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// Opening deps wires config, state, and registry client together
// and closes without error: the one call every command starts
// with. If this fails, commands boot half-wired.
func TestOpenDepsWiresFileStore(t *testing.T) {
	clearStoreEnv(t)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	t.Setenv(config.EnvRegistryURL, "http://127.0.0.1:1")
	d, err := OpenDeps()
	if err != nil {
		t.Fatalf("OpenDeps: %v", err)
	}
	if d.Cfg == nil || d.Store == nil || d.Reg == nil {
		t.Fatalf("deps = %+v, want config, store, and registry client", d)
	}
	if config.Current() != d.Cfg {
		t.Fatal("OpenDeps did not install its config as Current: use cases would read defaults")
	}
	d.Close()
}
