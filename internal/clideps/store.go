package clideps

import (
	"context"
	"fmt"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// StoreName voices which backend failed: the refusal names what the
// operator must fix, in either mode.
func StoreName(s store.Store) string {
	if _, ok := s.(*store.FileStore); ok {
		return "file store"
	}
	return "redis"
}

// OpenStore opens the derived backend, failing fast with a clear
// error: every keeper command needs state, and inventing numbers
// without it is worse than refusing. Exported so the e2e suite
// (test/e2e) opens state the same way every command does — auth, DB
// selection, and refusal included.
func OpenStore(cfg *config.Config) (store.StoreCloser, error) {
	backend, dir, err := ResolveStoreBackend()
	if err != nil {
		return nil, err
	}
	// NOTE(mutants): the 5s bound is timing, not logic — no test
	// distinguishes it from any other positive bound without a
	// stopwatch, and file Ping ignores ctx entirely. A mutant here
	// survives by being unobservable, not by being correct.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if backend == "file" {
		s := BuildStore(backend, dir, cfg)
		if err := s.Ping(ctx); err != nil {
			return nil, fmt.Errorf("file store at %s unreachable: %w", dir, err)
		}
		return s, nil
	}
	s := BuildStore(backend, dir, cfg)
	if err := s.Ping(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("redis unreachable at %s: %w", cfg.RedisAddr, err)
	}
	return s, nil
}

// BuildStore constructs the derived backend without probing it: one
// branch for OpenStore's fail-fast Ping and serve's lazy degrade, so
// a flipped conditional fails both instead of hiding in one.
func BuildStore(backend, dir string, cfg *config.Config) store.StoreCloser {
	if backend == "file" {
		return store.NewFileStore(dir)
	}
	return store.NewRedisStore(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
}
