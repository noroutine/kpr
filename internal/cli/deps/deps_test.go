package deps

import (
	"strings"
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
	if d.Backend != "file" || d.StoreDir == "" {
		t.Fatalf("deps backend = %q dir = %q, want the resolved backend recorded", d.Backend, d.StoreDir)
	}
	if config.Current() != d.Cfg {
		t.Fatal("OpenDeps did not install its config as Current: use cases would read defaults")
	}
	d.Close()
}

// Conflicting backend env refuses at OpenDeps: the resolution
// happens once here, so commands never re-derive it. If this
// fails, file+redis together boot half-wired.
func TestOpenDepsRefusesConflictingBackend(t *testing.T) {
	clearStoreEnv(t)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	if _, err := OpenDeps(); err == nil {
		t.Error("OpenDeps on conflicting backend succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("refusal = %q, want the conflict named", err.Error())
	}
}
