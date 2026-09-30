//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/registry"
)

// The gc sentinel must read a real writable registry as writable
// (cancelling its probe upload, so the probe repo never appears) and
// a real maintenance-readonly registry as readonly. If this fails, kpr
// gc either collects from live writes or refuses a ready registry.
func TestSentinelProbeModes(t *testing.T) {
	fx := NewFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if mode, err := gc.ProbeRegistryMode(ctx, fx.RegistryURL()); err != nil || mode != "writable" {
		t.Fatalf("sentinel on fixture registry = (%q, %v), want (writable, nil)", mode, err)
	}
	// No residue: the probe repo must not exist after the cancelled
	// initiate.
	if tags, err := registry.NewClient(fx.RegistryURL()).Catalog(ctx, gc.ProbeRepo); err == nil {
		t.Fatalf("probe repo catalog = %v, want absent (no residue)", tags)
	}

	roURL := startReadonlyRegistry(t)
	if mode, err := gc.ProbeRegistryMode(ctx, roURL); err != nil || mode != "readonly" {
		t.Fatalf("sentinel on readonly registry = (%q, %v), want (readonly, nil)", mode, err)
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
