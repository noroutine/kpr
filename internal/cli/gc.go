package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// probeRepo is the throwaway repo the gc sentinel uploads under. A
// cancelled initiate leaves no blob, no manifest, no residue.
const probeRepo = "kpr-gc-probe"

// gcProbe is what the write sentinel found: the registry takes writes,
// refuses them (v3 maintenance readonly), or answered something the
// probe cannot classify.
type gcProbe int

const (
	probeUnknown gcProbe = iota
	probeWritable
	probeReadonly
)

// registryBinPath is the stock registry binary gc shells out to. The
// image COPYs it from the same registry:3 the stack runs, so collector
// and store versions match by construction.
var registryBinPath = "/bin/registry"

// ProbeRegistryMode reports the sentinel verdict for baseURL as
// writable, readonly, or unknown (with the error). Exported so the e2e
// suite drives the same probe the CLI collects from — one path, never
// a copy.
func ProbeRegistryMode(ctx context.Context, baseURL string) (string, error) {
	mode, _, err := probeRegistry(ctx, baseURL)
	if err != nil {
		return "unknown", err
	}
	switch mode {
	case probeWritable:
		return "writable", nil
	case probeReadonly:
		return "readonly", nil
	default:
		return "unknown", fmt.Errorf("sentinel inconclusive for %s", baseURL)
	}
}

// probeRegistry initiates a blob upload under the probe repo: 202
// means writable (the upload is cancelled at once, leaving nothing),
// 405 means maintenance readonly. Anything else is inconclusive and
// an error — gc fails closed rather than collecting blind. The upload
// id returns with the writable verdict for the same-store proof.
func probeRegistry(ctx context.Context, baseURL string) (gcProbe, string, error) {
	endpoint := strings.TrimSuffix(baseURL, "/") + "/v2/" + probeRepo + "/blobs/uploads/"
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return probeUnknown, "", err
	}
	resp, err := client.Do(req) //nolint:gosec // operator-configured registry peer, no body
	if err != nil {
		return probeUnknown, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusAccepted:
		// Best effort: the upload never held content, and abandoned
		// uploads purge server-side — but cancel it anyway. Location
		// is usually absolute; resolve a relative one (tests, some
		// frontings) against the peer.
		uuid := uploadUUID(resp.Header.Get("Location"))
		if loc := resp.Header.Get("Location"); loc != "" {
			if !strings.HasPrefix(loc, "http://") && !strings.HasPrefix(loc, "https://") {
				loc = strings.TrimSuffix(baseURL, "/") + "/" + strings.TrimPrefix(loc, "/")
			}
			del, derr := http.NewRequestWithContext(ctx, http.MethodDelete, loc, nil)
			if derr == nil {
				dresp, derr := client.Do(del) //nolint:gosec // cancel of our own probe upload
				if derr == nil {
					_ = dresp.Body.Close()
				}
			}
		}
		return probeWritable, uuid, nil
	case http.StatusMethodNotAllowed:
		return probeReadonly, "", nil
	default:
		return probeUnknown, "", fmt.Errorf("sentinel POST %s: status %d, want 202 (writable) or 405 (readonly)", endpoint, resp.StatusCode)
	}
}

// gcArgs builds the stock collector invocation: the operator's flags,
// nothing invented.
func gcArgs(configPath string, deleteUntagged bool) []string {
	args := []string{"garbage-collect"}
	if deleteUntagged {
		args = append(args, "--delete-untagged")
	}
	return append(args, configPath)
}

// gcReady infers at runtime whether this container can collect at all:
// the stock binary and the registry config it reads store paths from
// must both exist. Anything missing refuses with the remedy — a bare
// image (no shared mounts) explains itself instead of failing mid-run.
func gcReady(binPath, configPath string) error {
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

// gcCacheReady dials the registry's blobdescriptor cache with the
// effective credentials: an unreachable cache mis-marks (live layers
// look unreferenced) and the run would delete what it must keep. No
// redis section means inmemory cache — nothing to gate.
func gcCacheReady(ctx context.Context, configPath string) error {
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

// registryStoreRoot parses the filesystem storage root out of the
// registry config: the local path the same-store proofs check. A
// config with no filesystem root (s3 and friends, garbage, absent)
// refuses — local collection only understands the shared directory
// layout.
func registryStoreRoot(configPath string) (string, error) {
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

// uploadUUID extracts the upload id from a blobs/uploads Location
// (absolute or relative, query stripped): the handle the same-store
// proof keys on. Unparseable locations prove nothing.
func uploadUUID(loc string) string {
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, s := range segs {
		if s == "uploads" && i+1 < len(segs) {
			return segs[i+1]
		}
	}
	return ""
}

// storeLayout joins the distribution filesystem layout below root.
func storeLayout(root string, elems ...string) string {
	return filepath.Join(append([]string{root, "docker", "registry", "v2"}, elems...)...)
}

// sameStoreUpload proves the probe upload landed in the local store:
// the uuid dir the registry just created must exist under the
// configured root. Retried briefly — creation races the response on a
// loaded registry; a minute-old absence means a different store.
func sameStoreUpload(root, uuid string) bool {
	if uuid == "" {
		return false
	}
	dir := storeLayout(root, "repositories", probeRepo, "_uploads", uuid)
	for i := 0; i < 5; i++ {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// sameStoreTagLink proves shared store without writing: a tracked
// tag's link file must resolve to the tracked digest. Readonly mode
// cannot mint fresh evidence, so it reuses what the receiver already
// recorded.
func sameStoreTagLink(root, repo, tag, digest string) bool {
	if repo == "" || tag == "" || digest == "" {
		return false
	}
	raw, err := os.ReadFile(storeLayout(root, "repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(raw)) == digest
}

// gcLockTTL bounds a collector run: a crashed gc releases at expiry
// instead of wedging every later run.
const gcLockTTL = 30 * time.Minute

// runGC probes the registry writable/readonly, proves the local mount
// is the registry's own store, then runs the stock collector against
// it under the shared collector lock. Writable without --force
// refuses: the operator flips storage.maintenance.readonly and
// restarts first. Anything unproven — inconclusive probe, invisible
// upload, unresolvable link — refuses always. After a successful
// collect the sentinel re-probes: a mode flip mid-run fails the run
// (writes may have raced the mark phase), a dead post-probe only
// warns. Flipping readonly stays with the operator; this command
// never rewrites registry config.
func runGC(ctx context.Context, w io.Writer, s store.Store, registryURL, configPath string, deleteUntagged, force bool) error {
	if err := gcReady(registryBinPath, configPath); err != nil {
		return err
	}
	root, err := registryStoreRoot(configPath)
	if err != nil {
		return err
	}
	if err := gcCacheReady(ctx, configPath); err != nil {
		return err
	}
	held, err := s.AcquireGCLock(ctx, gcLockTTL)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	if !held {
		return errors.New("another gc run holds the lock (kpr gc or make gc); wait it out or DEL kpr:gc:lock on the kpr redis DB if stale")
	}
	defer func() {
		if rerr := s.ReleaseGCLock(ctx); rerr != nil {
			_, _ = fmt.Fprintf(w, "Warning: gc lock release failed (%v); expires in %v\n", rerr, gcLockTTL)
		}
	}()
	mode, uuid, err := probeRegistry(ctx, registryURL)
	if err != nil {
		return err
	}
	switch mode {
	case probeWritable:
		if !sameStoreUpload(root, uuid) {
			return fmt.Errorf("sentinel upload %s not visible under %s: kpr does not share this registry's store", uuid, root)
		}
		if !force {
			return errors.New("registry is writable: enable storage.maintenance.readonly and restart it first, or re-run with --force accepting the risk")
		}
		if _, err := io.WriteString(w, "Warning: registry is writable; collecting anyway (--force)\n"); err != nil {
			return err
		}
	case probeReadonly:
		row, ok := firstDigestRow(ctx, s)
		if !ok {
			return errors.New("cannot prove shared store: no tracked digests; push or reap something first, or --force")
		}
		if !sameStoreTagLink(root, row.Repo, row.Tag, row.Digest) {
			return fmt.Errorf("tag %s:%s does not resolve to tracked %s under %s: kpr does not share this registry's store", row.Repo, row.Tag, row.Digest, root)
		}
		if _, err := fmt.Fprintf(w, "shared store proven via %s:%s\n", row.Repo, row.Tag); err != nil {
			return err
		}
	default:
		return fmt.Errorf("sentinel inconclusive for %s", registryURL)
	}
	cmd := exec.CommandContext(ctx, registryBinPath, gcArgs(configPath, deleteUntagged)...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("garbage-collect: %w", err)
	}
	post, _, perr := probeRegistry(ctx, registryURL)
	if perr != nil {
		_, _ = fmt.Fprintf(w, "Warning: post-run probe failed (%v); could not confirm the registry stayed %s\n", perr, modeName(mode))
		return nil
	}
	if post != mode {
		return fmt.Errorf("registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run", modeName(mode), modeName(post))
	}
	return nil
}

// modeName renders the sentinel verdict for messages.
func modeName(mode gcProbe) string {
	switch mode {
	case probeWritable:
		return "writable"
	case probeReadonly:
		return "readonly"
	default:
		return "unknown"
	}
}

// firstDigestRow returns any tracked row carrying a manifest digest.
func firstDigestRow(ctx context.Context, s store.Store) (policy.Row, bool) {
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

var gcConfigPath string

var gcDeleteUntagged bool

var gcForce bool

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect unreferenced registry blobs",
	Long: `Run the stock registry garbage-collect against the shared store.
The sentinel probes the registry first: readonly collects, writable
refuses unless --force (flip storage.maintenance.readonly and restart
it instead), inconclusive always refuses. Flipping readonly stays with
the operator — this command never rewrites registry config.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		s, err := OpenStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		return runGC(cmd.Context(), cmd.OutOrStdout(), s, cfg.RegistryURL, gcConfigPath, gcDeleteUntagged, gcForce)
	},
}

func init() {
	gcCmd.Flags().StringVar(&gcConfigPath, "config", "/etc/distribution/config.yml", "Registry config file (shared store paths come from it)")
	gcCmd.Flags().BoolVar(&gcDeleteUntagged, "delete-untagged", false, "Also drop orphaned manifests (same flag as registry garbage-collect)")
	gcCmd.Flags().BoolVar(&gcForce, "force", false, "Collect even when the sentinel finds the registry writable")
	RootCmd.AddCommand(gcCmd)
}
