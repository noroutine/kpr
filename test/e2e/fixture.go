//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"nrtn.dev/catalyst/kpr/internal/config"
)

// Backend images. Pinned like the compose stack (redis:8-alpine,
// registry:3) so scenarios prove the deployed pairing, not latest.
const (
	fixtureRedisImage    = "redis:8-alpine"
	fixtureRegistryImage = "registry:3"
)

// The fixture mirrors the dev stack's tenancy: a password-protected
// redis with kpr rows on DB 4 (DBs 0-2 belong to other tenants, 3 is
// the registry blobdescriptor cache). The password is e2e-only and
// differs from the dev default on purpose — scenarios must pass their
// own credentials through, never assume them.
const (
	fixtureRedisPassword = "kpr-e2e"
	fixtureRedisDB       = 4
)

// Fixture is a hermetic keeper backend pair: redis for TTL rows, a
// delete-enabled registry for manifests. One per test (containers are
// cheap, shared state is not); cleanup runs automatically.
type Fixture struct {
	redisAddr        string
	registryHostPort string
	registryLoopback string
	registryDirect   string
	registryURL      string
	// storageDir is the registry's storage on the host, empty when
	// the registry keeps it inside its own container. Only mount
	// fixtures (NewStorageFixture) set it — mount-reading use
	// cases (backfill) need the layout; the rest never touch it.
	storageDir string
}

// NewFixture starts the backend pair, skipping when no docker answers
// — the unit suite stays green without fixtures, and e2e never invents
// results. A container that fails to start is fatal, not a skip: once
// docker answers, failures are real.
func NewFixture(t *testing.T) *Fixture {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker unavailable, skipping e2e: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	redisC := startRedis(t, ctx)
	t.Cleanup(func() { testcontainers.CleanupContainer(t, redisC) })

	regC := startRegistry(t, ctx, "")
	t.Cleanup(func() { testcontainers.CleanupContainer(t, regC) })

	return assembleFixture(t, ctx, redisC, regC)
}

// startRedis runs the password-protected fixture redis.
func startRedis(t *testing.T, ctx context.Context) testcontainers.Container {
	t.Helper()
	redisC, err := testcontainers.Run(ctx, fixtureRedisImage,
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithCmd("redis-server", "--appendonly", "yes", "--requirepass", fixtureRedisPassword),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
	)
	if err != nil {
		t.Fatalf("start e2e redis: %v", err)
	}
	return redisC
}

// assembleFixture reads the endpoints off two running containers.
// Split out so mount fixtures reuse it without restating redis.
func assembleFixture(t *testing.T, ctx context.Context, redisC, regC testcontainers.Container) *Fixture {
	t.Helper()
	redisHost, err := redisC.Host(ctx)
	if err != nil {
		t.Fatalf("redis host: %v", err)
	}
	redisPort, err := redisC.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("redis port: %v", err)
	}
	regHost, err := regC.Host(ctx)
	if err != nil {
		t.Fatalf("registry host: %v", err)
	}
	regPort, err := regC.MappedPort(ctx, "5000/tcp")
	if err != nil {
		t.Fatalf("registry port: %v", err)
	}
	info, err := regC.Inspect(ctx)
	if err != nil {
		t.Fatalf("registry inspect: %v", err)
	}
	// Container IP: how co-located containers (the toolbox) reach the
	// registry without touching published ports at all. Modern
	// engines report it per-network, not top-level.
	direct := ""
	for _, net := range info.NetworkSettings.Networks {
		if net.IPAddress.IsValid() {
			direct = net.IPAddress.String()
			break
		}
	}
	if direct == "" {
		t.Fatalf("registry has no container IP")
	}
	hostPort := regHost + ":" + regPort.Port()
	return &Fixture{
		redisAddr:        redisHost + ":" + redisPort.Port(),
		registryHostPort: hostPort,
		registryLoopback: "localhost:" + regPort.Port(),
		registryDirect:   direct + ":5000",
		registryURL:      "http://" + hostPort,
	}
}

// NewStorageFixture is NewFixture with the registry's storage on a
// host temp dir, bind-mounted at the stock rootdirectory. The test
// process reads the same layout the registry writes — the compose
// shadow-registry overlay's trick (an unwired door onto shared
// storage), minus the second container: e2e has no receiver, so a
// push the scenario doesn't record IS a pre-kpr tag. Scenarios that
// never read the mount use NewFixture and pay for no bind.
func NewStorageFixture(t *testing.T) *Fixture {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker unavailable, skipping e2e: %v", err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	redisC := startRedis(t, ctx)
	t.Cleanup(func() { testcontainers.CleanupContainer(t, redisC) })
	regC := startRegistry(t, ctx, dir)
	t.Cleanup(func() { testcontainers.CleanupContainer(t, regC) })
	fx := assembleFixture(t, ctx, redisC, regC)
	fx.storageDir = dir
	return fx
}

// startRegistry runs the delete-enabled fixture registry, optionally
// with hostDir bind-mounted as its storage. A mount failure is
// fatal: file sharing off means the mount-reading scenario can
// prove nothing.
func startRegistry(t *testing.T, ctx context.Context, hostDir string) testcontainers.Container {
	t.Helper()
	opts := []testcontainers.ContainerCustomizer{
		testcontainers.WithExposedPorts("5000/tcp"),
		testcontainers.WithEnv(map[string]string{"REGISTRY_STORAGE_DELETE_ENABLED": "true"}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/v2/").WithPort("5000/tcp")),
	}
	if hostDir != "" {
		opts = append(opts, testcontainers.WithMounts(testcontainers.ContainerMount{
			Source: testcontainers.GenericBindMountSource{HostPath: hostDir},
			Target: "/var/lib/registry",
		}))
	}
	regC, err := testcontainers.Run(ctx, fixtureRegistryImage, opts...)
	if err != nil {
		t.Fatalf("start e2e registry: %v", err)
	}
	return regC
}

// RedisAddr is the kpr state backend (host:port).
func (f *Fixture) RedisAddr() string { return f.redisAddr }

// RedisPassword authenticates the fixture redis.
func (f *Fixture) RedisPassword() string { return fixtureRedisPassword }

// RedisDB is kpr's logical database on the fixture redis.
func (f *Fixture) RedisDB() int { return fixtureRedisDB }

// StorageDir is the registry's storage as a host path — the root
// backfill stats tag links under. Empty on plain fixtures, whose
// registries keep storage inside their own containers.
func (f *Fixture) StorageDir() string { return f.storageDir }

// RegistryURL is the registry base URL (deletes enabled).
func (f *Fixture) RegistryURL() string { return f.registryURL }

// RegistryHostPort is the registry without scheme, for image refs.
func (f *Fixture) RegistryHostPort() string { return f.registryHostPort }

// RegistryLoopback is the registry as localhost:port — for clients
// that only speak to loopback over plain HTTP (the docker daemon).
func (f *Fixture) RegistryLoopback() string { return f.registryLoopback }

// RegistryDirect is the registry by container IP — for clients
// running alongside the fixtures (the toolbox), bypassing published
// ports entirely.
func (f *Fixture) RegistryDirect() string { return f.registryDirect }

// stageCollectorStub writes a stub collector binary: it runs the
// prologue (a sleep to hold a lease window, or nothing), then
// records one finished run per line in runs. The collect seam is
// package-private, so e2e drives the binary, never the func —
// callers count lines to assert how many collects finished.
func stageCollectorStub(t *testing.T, prologue string) (bin, runs string) {
	t.Helper()
	runs = filepath.Join(t.TempDir(), "collector-runs.log")
	bin = filepath.Join(t.TempDir(), "collector-stub")
	script := "#!/bin/sh\n" + prologue + "\nprintf 'run\\n' >> '" + runs + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("stage collector stub: %v", err)
	}
	return bin, runs
}

// collectorRuns counts finished stub collects: the runs log grows
// one line per exit-0 run, so a refused or crashed collect counts
// nothing.
func collectorRuns(t *testing.T, runs string) int {
	t.Helper()
	raw, err := os.ReadFile(runs)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read collector runs: %v", err)
	}
	return len(strings.Split(strings.TrimSpace(string(raw)), "\n"))
}

// stageRunConfig points config.Current at one run's world —
// registry, mount, stub collector, time server — and returns the
// restore. gc, unlock, and backfill resolve everything else off
// Current, the way production reads it.
func stageRunConfig(t *testing.T, url, cfg, bin, timeserver, edgeAddr string) func() {
	t.Helper()
	// TimeServer rides env only (no builder knob): set it before
	// building Current, the way a deployment exports it.
	t.Setenv(config.EnvTimeServer, timeserver)
	return config.SetCurrent(config.NewBuilder().FromEnv().
		WithRegistryURL(url).
		WithRegistryConfig(cfg).
		WithRegistryBinPath(bin).
		WithEdgeAddr(edgeAddr).
		Build())
}
