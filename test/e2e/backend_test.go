//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/testing/storetest"
)

// The redis backend implements the shared store contract against the
// real thing — auth, kpr's DB, the lot — via the fixture, never
// ambient localhost. If this fails, the file contract below is
// proving one backend while production runs another.
func TestRedisStoreContract(t *testing.T) {
	fx := NewFixture(t)
	s := store.NewRedisStore(fx.RedisAddr(), fx.RedisPassword(), fx.RedisDB())
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("fixture redis unreachable: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	flush := func(t *testing.T) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
		defer cancel()
		if err := s.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	storetest.RunContract(t, func(t *testing.T) store.Store { flush(t); return s })
}

// The file backend implements the same contract with no fixture at
// all — a pristine dir per scenario (cleaner than flush, and the
// point: no shared state between scenarios, only the layout). Both
// production backends prove the suite in this gate; mem stays a
// unit-test stand-in. If this fails, redis-less mode reasons
// differently about rows, marks, or locks.
func TestFileStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) store.Store {
		t.Helper()
		return store.NewFileStore(t.TempDir())
	})
}

// Opening state the way every keeper command does succeeds against
// the fixture — and a wrong password refuses with a naming-redis
// error instead of a usable store. If this fails, the CLI's Ping gate
// passes what it should refuse, or refuses what it should pass.
func TestOpenStoreAgainstFixture(t *testing.T) {
	// Backend selection is ambient env (KPR_STORE): pin redis, or a
	// default-env run opens the file backend and the wrong password
	// below proves nothing — this test failed exactly that way.
	t.Setenv(config.EnvStore, "redis")
	fx := NewFixture(t)
	cfg := config.NewBuilder().
		WithRedisAddr(fx.RedisAddr()).
		WithRedisPassword(fx.RedisPassword()).
		WithRedisDB(fx.RedisDB()).
		Build()

	backend, dir, err := deps.ResolveStoreBackend()
	if err != nil {
		t.Fatalf("resolve backend: %v", err)
	}
	s, err := deps.OpenStore(cfg, backend, dir)
	if err != nil {
		t.Fatalf("OpenStore with fixture credentials: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		t.Errorf("opened store does not ping: %v", err)
	}
	_ = s.Close()

	bad := config.NewBuilder().
		WithRedisAddr(fx.RedisAddr()).
		WithRedisPassword("wrong").
		WithRedisDB(fx.RedisDB()).
		Build()
	rs, err := deps.OpenStore(bad, backend, dir)
	if err == nil {
		_ = rs.Close()
		t.Fatal("OpenStore with wrong password succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Errorf("refusal = %q, want it to name redis", err.Error())
	}
}
