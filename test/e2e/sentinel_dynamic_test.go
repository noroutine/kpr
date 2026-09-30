//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// A generation written straight to the store must come back through
// the API: manifest by tag, payload blob behind it. No API writes
// anywhere — the bind-mounted store is the only channel. If this
// fails, fs-crafted evidence is invisible to the registry (layout
// drift, or a cache sitting between the API and the disk).
func TestSentinelDynamicRoundTrip(t *testing.T) {
	url, store := startMountedRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	api := registry.NewClient(url)

	if _, _, err := sentinel.Read(ctx, api, "kpr-sentinel", "live"); err == nil {
		t.Fatal("read before write succeeded, want absence")
	}
	want := sentinel.Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000003", TS: time.Now().UTC().Format(time.RFC3339), Writer: "e2e"}
	if _, err := sentinel.Write(store, "kpr-sentinel", "live", want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _, err := sentinel.Read(ctx, api, "kpr-sentinel", "live")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("payload = %+v, want %+v", got, want)
	}
	// A new generation repoints the tag: the API must serve it at
	// once, never a cached older one. If this fails, updates are
	// write-only and the proof compares stale evidence.
	want.Gen = "0193abcd-0000-7000-8000-000000000004"
	if _, err := sentinel.Write(store, "kpr-sentinel", "live", want); err != nil {
		t.Fatalf("Write gen 4: %v", err)
	}
	if err := sentinel.Verify(ctx, api, "kpr-sentinel", "live", "0193abcd-0000-7000-8000-000000000004"); err != nil {
		t.Errorf("Verify(gen 4) = %v, want nil", err)
	}
}

// startMountedRegistry boots a registry:3 with its store on a host
// temp dir the test writes directly — the same shared layout the gc
// container sees in production. Returns the base URL and the store.
func startMountedRegistry(t *testing.T) (string, string) {
	t.Helper()
	store := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	regC, err := testcontainers.Run(ctx, fixtureRegistryImage,
		testcontainers.WithExposedPorts("5000/tcp"),
		testcontainers.WithEnv(map[string]string{"REGISTRY_STORAGE_DELETE_ENABLED": "true"}),
		testcontainers.WithMounts(testcontainers.BindMount(store, "/var/lib/registry")),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/v2/").WithPort("5000/tcp")),
	)
	if err != nil {
		t.Fatalf("start mounted registry: %v", err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, regC) })
	host, err := regC.Host(ctx)
	if err != nil {
		t.Fatalf("registry host: %v", err)
	}
	port, err := regC.MappedPort(ctx, "5000/tcp")
	if err != nil {
		t.Fatalf("registry port: %v", err)
	}
	return "http://" + host + ":" + port.Port(), store
}
