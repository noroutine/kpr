// Package fakes holds test fakes for the shared-store world: a
// file-backed registry API, a scripted clock, staged config, and
// the fault doubles. Any suite proving store ceremonies stands on
// the same ground — one set of fakes, never a copy per suite.
package fakes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// FileAPI serves the registry API off a directory layout: the fake
// registry the proofs read back through.
type FileAPI struct{ Root string }

func (f FileAPI) blob(root, digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	return os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", hex[:2], hex, "data"))
}

func (f FileAPI) GetManifest(_ context.Context, repo, tag string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(f.Root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return nil, err
	}
	return f.blob(f.Root, strings.TrimSpace(string(raw)))
}

func (f FileAPI) GetBlob(_ context.Context, repo, digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	if _, err := os.Stat(filepath.Join(f.Root, "docker", "registry", "v2", "repositories", repo, "_layers", "sha256", hex, "link")); err != nil {
		return nil, err
	}
	return f.blob(f.Root, digest)
}

// StubClock answers a fixed offset (or error): the verdict-on-offset
// wiring without the network. Zero value is a healthy clock.
type StubClock struct {
	Off time.Duration
	Err error
}

func (s StubClock) Offset(context.Context, string) (time.Duration, error) {
	if s.Err != nil {
		return 0, s.Err
	}
	return s.Off, nil
}

var ErrClockUnreachable = errors.New("no route to time source")

// Config installs the Current the run reads: the registry URL and
// config file the test staged, the fixed example time source, and
// the collector binary (/bin/sh unless the test stages its own).
// Scoped to the test — SetCurrent restores after.
func Config(t *testing.T, url, cfgPath string, bin ...string) {
	t.Helper()
	binPath := "/bin/sh"
	if len(bin) > 0 {
		binPath = bin[0]
	}
	t.Cleanup(config.SetCurrent(&config.Config{
		RegistryURL: url, RegistryConfig: cfgPath, TimeServer: "time.example.com",
		RegistryBinPath: binPath,
	}))
}

// ProvenRun stages a config over an empty root and returns the
// config, the root, and a lock: the sentinel Write inside the run
// lays the proof ground itself, so tests start empty and vary one
// port. Runs prove locality per pass, but intent is the
// operator's: staged stores arrive unlocked so tests vary the
// ports, not the marker. Fresh-locked is pinned by the storetest
// contract and the dedicated refusal test.
func ProvenRun(t *testing.T) (string, string, *store.MemStore) {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	s := store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	return cfg, root, s
}

func NewGenID(t *testing.T) string {
	t.Helper()
	id, err := sentinel.NewGen()
	if err != nil {
		t.Fatalf("mint generation id: %v", err)
	}
	return id
}

// PairedGen stages a served generation over root and pairs the lock
// to it: the settled ground every proof ceremony starts from.
func PairedGen(t *testing.T, lock *store.MemStore, root string) string {
	t.Helper()
	gen := NewGenID(t)
	id := NewGenID(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: id, TS: now}); err != nil {
		t.Fatalf("stage paired generation: %v", err)
	}
	if err := lock.SetIdentity(context.Background(), store.Identity{
		ID: id, BaselineGen: gen, AdoptedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("pair store: %v", err)
	}
	return gen
}

// NewRow tracks a generation at a fixed age: stale verdicts compare
// against these.
func NewRow(repo, tag string, at time.Time) policy.Row {
	return policy.Row{Repo: repo, Tag: tag, Digest: "sha256:" + tag,
		MediaType: sentinel.ManifestMediaType, PushedAt: at, Actor: "test"}
}

// ErrWriter fails every write: the broken-pipe stand-in.
type ErrWriter struct{ Err error }

func (w ErrWriter) Write([]byte) (int, error) { return 0, w.Err }

var ErrTestStoreDown = errors.New("redis: connection refused")

// FailIdentityStore fails the pairing write once armed: staging
// pairs through it, the ceremony hits the outage.
type FailIdentityStore struct {
	*store.MemStore
	Armed bool
}

func (f *FailIdentityStore) SetIdentity(ctx context.Context, id store.Identity) error {
	if f.Armed {
		return ErrTestStoreDown
	}
	return f.MemStore.SetIdentity(ctx, id)
}

// ErrIdentityStore fails the lineage read: the backend-outage
// stand-in for the pairing record.
type ErrIdentityStore struct{ Err error }

func (e ErrIdentityStore) GetIdentity(context.Context) (store.Identity, error) {
	return store.Identity{}, e.Err
}

func (e ErrIdentityStore) SetIdentity(context.Context, store.Identity) error { return e.Err }

// ErrRecorder fails the keep-N write: the backend-outage stand-in
// for the generation log.
type ErrRecorder struct{ Err error }

func (e ErrRecorder) Record(context.Context, policy.Row) error { return e.Err }

// FailRows fails the prune reads/writes: the backend-outage
// stand-in for the old-epoch sweep.
type FailRows struct {
	*store.MemStore
	AllErr error
	DelErr error
}

func (f FailRows) All(ctx context.Context) ([]policy.Row, error) {
	if f.AllErr != nil {
		return nil, f.AllErr
	}
	return f.MemStore.All(ctx)
}

func (f FailRows) Delete(ctx context.Context, repo, tag string) error {
	if f.DelErr != nil {
		return f.DelErr
	}
	return f.MemStore.Delete(ctx, repo, tag)
}

// MemLeaseConn is the lease conn surface over a map: the shared
// conformance's second medium beside the filesystem, and the
// redis branch without a live server. It records TTLs without
// honoring them — overrun reads stay testable without a clock,
// while TTL assertions read back what Hold asked for.
type MemLeaseConn struct {
	rows map[string][]byte
	ttls map[string]time.Duration
}

// NewMemLeaseConn stages an empty lease surface.
func NewMemLeaseConn() *MemLeaseConn {
	return &MemLeaseConn{rows: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (m *MemLeaseConn) Get(_ context.Context, key string) ([]byte, error) {
	raw, ok := m.rows[key]
	if !ok {
		// A miss answers (nil, nil): the adapter contract for a
		// redis Nil, and a missing file reads the same. An error
		// here would mean outage, and the read would log it.
		return nil, nil
	}
	return raw, nil
}

func (m *MemLeaseConn) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	m.rows[key] = val
	m.ttls[key] = ttl
	return nil
}

func (m *MemLeaseConn) Del(_ context.Context, key string) error {
	delete(m.rows, key)
	delete(m.ttls, key)
	return nil
}

// TTL reports the last hygiene horizon Hold asked for.
func (m *MemLeaseConn) TTL(key string) time.Duration {
	return m.ttls[key]
}
