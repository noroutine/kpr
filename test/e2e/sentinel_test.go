//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// The gc sentinel must read a real writable registry as writable
// (cancelling its probe upload, so the probe repo never appears) and
// a real maintenance-readonly registry as readonly. The probe seam
// is package-private, so the modes read off preview output: the
// preview names the probed mode, then refuses the unpaired store —
// the mode line is the assertion, the refusal is the pairing rule.
// If this fails, kpr gc either collects from live writes or refuses
// a ready registry.
func TestSentinelProbeModes(t *testing.T) {
	// Storage fixture: the preview proves the filesystem store
	// before probing, so the mount root must be real.
	fx := NewStorageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := stageRegistryConfig(t, fx.StorageDir())
	bin, _ := stageCollectorStub(t, "")
	previewMode := func(url, cfg string) (string, error) {
		st := store.NewFileStore(t.TempDir())
		if err := st.SetUnlocked(ctx, true); err != nil {
			t.Fatalf("unlock preview store: %v", err)
		}
		restore := stageRunConfig(t, url, cfg, bin, stageTimeServer(t), "")
		defer restore()
		var out strings.Builder
		// Report stays nil: the mode line under test renders
		// through the default reporter into out.
		err := gc.Run(ctx, &out, gc.Deps{
			Lock: st, Rec: st, Ids: st, Rows: st,
			API: registry.NewClient(url), Clock: clock.HTTPS{},
			Store: st,
		}, gc.Options{}, gc.Accepts{})
		return out.String(), err
	}

	out, err := previewMode(fx.RegistryURL(), cfg)
	if !strings.Contains(out, "registry is WRITABLE") {
		t.Errorf("preview names no writable mode:\n%s", out)
	}
	if err == nil || !strings.Contains(err.Error(), "no sentinel") {
		t.Errorf("preview over unpaired store = %v, want the no-sentinel refusal", err)
	}
	// No residue: the probe repo must not exist after the cancelled
	// initiate (the product const lives in gc; this literal pins
	// the contract from outside).
	const probeRepo = "noroutine/kpr-gc-probe"
	if tags, err := registry.NewClient(fx.RegistryURL()).Catalog(ctx, probeRepo); err == nil {
		t.Fatalf("probe repo catalog = %v, want absent (no residue)", tags)
	}

	roURL := startReadonlyRegistry(t)
	// The readonly registry's storage lives inside its container:
	// the preview never walks the mount (no collect, no prune),
	// so the proof takes a parseable empty root, never a lie
	// about shared storage.
	roOut, roErr := previewMode(roURL, stageRegistryConfig(t, t.TempDir()))
	if !strings.Contains(roOut, "registry is READONLY") {
		t.Errorf("preview names no readonly mode:\n%s", roOut)
	}
	if roErr == nil {
		t.Errorf("preview over unpaired readonly store succeeded, want the pairing refusal")
	}
}

// readonlyRegistryConfig is the v3 maintenance-readonly shape the file
// oracle proved (the REGISTRY_STORAGE_MAINTENANCE_READONLY_ENABLED env
// override panics registry:3 — readonly goes in the config file).
const readonlyRegistryConfig = `version: 0.1
log:
  level: warn
storage:
  cache:
    blobdescriptor: inmemory
  filesystem:
    rootdirectory: /var/lib/registry
  maintenance:
    readonly:
      enabled: true
http:
  addr: :5000
`

// startReadonlyRegistry boots a throwaway registry:3 in maintenance
// readonly (mounted config file) and returns its base URL.
func startReadonlyRegistry(t *testing.T) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte(readonlyRegistryConfig), 0o644); err != nil {
		t.Fatalf("stage readonly config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	regC, err := testcontainers.Run(ctx, fixtureRegistryImage,
		testcontainers.WithExposedPorts("5000/tcp"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      cfgPath,
			ContainerFilePath: "/etc/distribution/config.yml",
			FileMode:          0o644,
		}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/v2/").WithPort("5000/tcp")),
	)
	if err != nil {
		t.Fatalf("start readonly registry: %v", err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, regC) })
	host, err := regC.Host(ctx)
	if err != nil {
		t.Fatalf("readonly host: %v", err)
	}
	port, err := regC.MappedPort(ctx, "5000/tcp")
	if err != nil {
		t.Fatalf("readonly port: %v", err)
	}
	return "http://" + host + ":" + port.Port()
}
