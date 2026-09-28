//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
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
	registryURL      string
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

	redisC, err := testcontainers.Run(ctx, fixtureRedisImage,
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithCmd("redis-server", "--appendonly", "yes", "--requirepass", fixtureRedisPassword),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
	)
	if err != nil {
		t.Fatalf("start e2e redis: %v", err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, redisC) })

	regC, err := testcontainers.Run(ctx, fixtureRegistryImage,
		testcontainers.WithExposedPorts("5000/tcp"),
		testcontainers.WithEnv(map[string]string{"REGISTRY_STORAGE_DELETE_ENABLED": "true"}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/v2/").WithPort("5000/tcp")),
	)
	if err != nil {
		t.Fatalf("start e2e registry: %v", err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, regC) })

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
	hostPort := regHost + ":" + regPort.Port()
	return &Fixture{
		redisAddr:        redisHost + ":" + redisPort.Port(),
		registryHostPort: hostPort,
		registryLoopback: "localhost:" + regPort.Port(),
		registryURL:      "http://" + hostPort,
	}
}

// RedisAddr is the kpr state backend (host:port).
func (f *Fixture) RedisAddr() string { return f.redisAddr }

// RedisPassword authenticates the fixture redis.
func (f *Fixture) RedisPassword() string { return fixtureRedisPassword }

// RedisDB is kpr's logical database on the fixture redis.
func (f *Fixture) RedisDB() int { return fixtureRedisDB }

// RegistryURL is the registry base URL (deletes enabled).
func (f *Fixture) RegistryURL() string { return f.registryURL }

// RegistryHostPort is the registry without scheme, for image refs.
func (f *Fixture) RegistryHostPort() string { return f.registryHostPort }

// RegistryLoopback is the registry as localhost:port — for clients
// that only speak to loopback over plain HTTP (the docker daemon).
func (f *Fixture) RegistryLoopback() string { return f.registryLoopback }
