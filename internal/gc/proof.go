package gc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.yaml.in/yaml/v3"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

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
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db, DialTimeout: 2 * time.Second})
	defer func() { _ = rdb.Close() }()
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

// layout joins the distribution filesystem layout below root.
func layout(root string, elems ...string) string {
	return filepath.Join(append([]string{root, "docker", "registry", "v2"}, elems...)...)
}

// SameStoreUpload proves the probe upload landed in the local store:
// the uuid dir the registry just created must exist under the
// configured root. Retried briefly — creation races the response on a
// loaded registry; a minute-old absence means a different store.
func SameStoreUpload(root, uuid string) bool {
	if uuid == "" {
		return false
	}
	dir := layout(root, "repositories", ProbeRepo, "_uploads", uuid)
	for i := 0; i < 5; i++ {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// SameStoreTagLink proves shared store without writing: a tracked
// tag's link file must resolve to the tracked digest. Readonly mode
// cannot mint fresh evidence, so it reuses what the receiver already
// recorded.
func SameStoreTagLink(root, repo, tag, digest string) bool {
	if repo == "" || tag == "" || digest == "" {
		return false
	}
	raw, err := os.ReadFile(layout(root, "repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(raw)) == digest
}

// FirstDigestRow returns any tracked row carrying a manifest digest:
// readonly proofs need one recorded link to check.
func FirstDigestRow(ctx context.Context, s store.Store) (policy.Row, bool) {
	rows, err := s.All(ctx)
	if err != nil {
		return policy.Row{}, false
	}
	for _, r := range rows {
		if r.Digest != "" {
			return r, true
		}
	}
	return policy.Row{}, false
}
