package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
	"go.yaml.in/yaml/v3"

	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// writeVerifiedGeneration is the shared tail of every mint ceremony
// (armed gc runs, unlock): one generation written and read back,
// refusing identically everywhere — one funnel, no copies. Returns
// the manifest digest for keep-N. The future FreshGeneration token
// is minted here, beside the Write and Verify it names; until its
// first consumer arrives it stays parked, not faked.
func writeVerifiedGeneration(ctx context.Context, api sentinel.API, root string, payload sentinel.Payload) (string, error) {
	md, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, payload)
	if err != nil {
		return "", fmt.Errorf("sentinel generation unwritable under %s: %w", root, err)
	}
	if err := sentinel.Verify(ctx, api, sentinel.Repo, sentinel.Tag, payload.Gen); err != nil {
		return "", fmt.Errorf("kpr does not share this registry's store: %v", err)
	}
	return md, nil
}

// collectWritableArmed runs the collector for real against a
// writable registry: acceptance is demanded at the delete boundary,
// not just at the pre-mint gate (a run that arrives here without it
// refuses instead of collecting blind). Previews and readonly runs
// take the bare collect — only the writable-armed path carries the
// extra demand, which is why only it has a variant.
func collectWritableArmed(ctx context.Context, out io.Writer, collect Collector, binPath string, args []string, report Reporter, risk proof.AcceptedRisk) error {
	if risk == nil {
		return errors.New("registry is writable: enable storage.maintenance.readonly and restart it first, or re-run with --force accepting the risk")
	}
	return collect(ctx, out, binPath, args, report)
}

// Ready infers at runtime whether this container can collect at all:
// the stock binary and the registry config it reads store paths from
// must both exist. Anything missing refuses with the remedy — a bare
// image (no shared mounts) explains itself instead of failing mid-run.
func Ready(binPath, configPath string) error {
	if st, err := os.Stat(binPath); err != nil || st.IsDir() {
		return fmt.Errorf("gc unavailable: registry binary not found at %s (image must COPY it from the registry image)", binPath)
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("gc unavailable: registry config not found at %s (mount the registry config here, see --config)", configPath)
	}
	return nil
}

// registryRedis parses the blobdescriptor cache endpoint out of the
// registry config: first addr (cluster list or single), password with
// the REGISTRY_REDIS_PASSWORD override the collector itself honors,
// db. Empty addr means no redis cache (inmemory) — nothing to gate.
func registryRedis(configPath string) (addr, password string, db int, err error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", "", 0, err
	}
	var cfg struct {
		Redis struct {
			Addrs    []string `yaml:"addrs"`
			Addr     string   `yaml:"addr"`
			Password string   `yaml:"password"`
			DB       int      `yaml:"db"`
		} `yaml:"redis"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return "", "", 0, fmt.Errorf("parse %s: %w", configPath, err)
	}
	addr = cfg.Redis.Addr
	if len(cfg.Redis.Addrs) > 0 {
		addr = cfg.Redis.Addrs[0]
	}
	if addr == "" {
		return "", "", 0, nil
	}
	password = cfg.Redis.Password
	if env, ok := os.LookupEnv("REGISTRY_REDIS_PASSWORD"); ok {
		password = env
	}
	return addr, password, cfg.Redis.DB, nil
}

// CacheReady dials the registry's blobdescriptor cache with the
// effective credentials: an unreachable cache mis-marks (live layers
// look unreferenced) and the run would delete what it must keep. No
// redis section means inmemory cache — nothing to gate.
func CacheReady(ctx context.Context, configPath string) error {
	addr, password, db, err := registryRedis(configPath)
	if err != nil {
		return err
	}
	if addr == "" {
		return nil
	}
	// NOTE(mutants): timeout arithmetic is equivalent — no test
	// distinguishes a 2s dial from a 3s one, and none should.
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db, DialTimeout: 2 * time.Second})
	defer func() { _ = rdb.Close() }()
	// NOTE(mutants): same — ping timeout arithmetic is equivalent.
	ping, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ping).Err(); err != nil {
		return fmt.Errorf("blobdescriptor cache unreachable at %s: %w", addr, err)
	}
	return nil
}

// StoreRoot parses the filesystem storage root out of the registry
// config: the local path the same-store proofs check. A config with
// no filesystem root (s3 and friends, garbage, absent) refuses —
// local collection only understands the shared directory layout.
func StoreRoot(configPath string) (string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var cfg struct {
		Storage struct {
			Filesystem struct {
				RootDirectory string `yaml:"rootdirectory"`
			} `yaml:"filesystem"`
		} `yaml:"storage"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", configPath, err)
	}
	if cfg.Storage.Filesystem.RootDirectory == "" {
		return "", fmt.Errorf("no filesystem storage root in %s: local gc needs the shared directory layout", configPath)
	}
	return cfg.Storage.Filesystem.RootDirectory, nil
}
