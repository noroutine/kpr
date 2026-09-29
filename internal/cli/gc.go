package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// registryBinPath is the stock registry binary gc shells out to. The
// image COPYs it from the same registry:3 the stack runs, so collector
// and store versions match by construction.
var registryBinPath = "/bin/registry"

// gcArgs builds the stock collector invocation: the operator's flags,
// nothing invented. dryRun previews (the default); only an explicit
// --no-dry-run collects for real.
func gcArgs(configPath string, deleteUntagged, dryRun bool) []string {
	args := []string{"garbage-collect"}
	if dryRun {
		args = append(args, "--dry-run")
	}
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
	dir := storeLayout(root, "repositories", gc.ProbeRepo, "_uploads", uuid)
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

// GCOptions tunes a gc run: the operator's flags plus the event
// reporter the CLI renders loud. DryRun previews (the default) —
// only an explicit --no-dry-run collects for real.
type GCOptions struct {
	DeleteUntagged bool
	Force          bool
	DryRun         bool
	Report         GCReporter
}

// runGC probes the registry writable/readonly, proves the local mount
// is the registry's own store, then runs the stock collector against
// it under the shared collector lock, streaming every line and
// reporting each stage. A real (non-dry) run on a writable registry
// refuses: the operator flips storage.maintenance.readonly and
// restarts first. A dry-run preview on a writable registry proceeds
// with a loud warning instead — previews delete nothing, so a race
// only stales the preview. Anything unproven — inconclusive probe,
// invisible upload, unresolvable link — refuses always. After the
// collect the sentinel re-probes: a mode flip mid-run is loud but
// never a panic — without --force it fails the run (writes may have
// raced the mark phase), with --force the operator presumed to know
// and the run passes warned. A dead post-probe only warns.
// Flipping readonly stays with the operator; this command never
// rewrites registry config.
func runGC(ctx context.Context, w io.Writer, s store.Store, registryURL, configPath string, opts GCOptions) error {
	gcStarted := time.Now()
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
	held, err := s.AcquireLock(ctx, store.GCLockKey, gcLockTTL)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	if !held {
		return errors.New("another gc run holds the lock (kpr gc or make gc); wait it out or DEL kpr:gc:lock on the kpr redis DB if stale")
	}
	defer func() {
		if rerr := s.ReleaseLock(ctx, store.GCLockKey); rerr != nil {
			_, _ = fmt.Fprintf(w, "Warning: gc lock release failed (%v); expires in %v\n", rerr, gcLockTTL)
		}
	}()
	mode, uuid, err := gc.ProbeRegistry(ctx, registryURL)
	if err != nil {
		return err
	}
	pre := timedGCEvent(GCStagePreProbe, gcStarted)
	pre.Message = gc.ModeName(mode)
	emitGC(opts.Report, pre)
	switch mode {
	case gc.ModeWritable:
		if !sameStoreUpload(root, uuid) {
			return fmt.Errorf("sentinel upload %s not visible under %s: kpr does not share this registry's store", uuid, root)
		}
		if opts.DryRun {
			if _, err := io.WriteString(w, "Warning: registry is writable; preview only, nothing will be deleted\n"); err != nil {
				return err
			}
		} else {
			if !opts.Force {
				return errors.New("registry is writable: enable storage.maintenance.readonly and restart it first, or re-run with --force accepting the risk")
			}
			if _, err := io.WriteString(w, "Warning: registry is writable; collecting anyway (--force)\n"); err != nil {
				return err
			}
		}
	case gc.ModeReadonly:
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
	if err := runCollector(ctx, w, registryBinPath, gcArgs(configPath, opts.DeleteUntagged, opts.DryRun), opts.Report); err != nil {
		return err
	}
	post, _, perr := gc.ProbeRegistry(ctx, registryURL)
	if perr != nil {
		_, _ = fmt.Fprintf(w, "Warning: post-run probe failed (%v); could not confirm the registry stayed %s\n", perr, gc.ModeName(mode))
		return nil
	}
	pev := timedGCEvent(GCStagePostProbe, gcStarted)
	pev.Message = gc.ModeName(post)
	emitGC(opts.Report, pev)
	if post != mode {
		flip := timedGCEvent(GCStageModeFlip, gcStarted)
		flip.Message = gc.ModeName(mode) + "→" + gc.ModeName(post)
		emitGC(opts.Report, flip)
		if _, werr := fmt.Fprintf(w, "WARNING: registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run\n", gc.ModeName(mode), gc.ModeName(post)); werr != nil {
			return werr
		}
		if !opts.Force {
			return fmt.Errorf("registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run", gc.ModeName(mode), gc.ModeName(post))
		}
	}
	return nil
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

var gcNoDryRun bool

// renderGCEvent voices the lifecycle loud: probe verdicts, collector
// start (pid, so a long mark phase is visibly alive), post-probe, and
// the flip banner. Collector lines stream raw alongside.
func renderGCEvent(w io.Writer, dryRun bool) GCReporter {
	return func(e GCEvent) {
		switch e.Stage {
		case GCStagePreProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry is %s\n", strings.ToUpper(e.Message))
		case GCStageStarted:
			if e.PID != 0 {
				_, _ = fmt.Fprintf(w, "collector started (pid %d)%s\n", e.PID, drySuffix(dryRun))
			}
		case GCStagePostProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry still %s\n", strings.ToUpper(e.Message))
		case GCStageModeFlip:
			_, _ = fmt.Fprintf(w, "WARNING: registry flipped %s mid-run\n", e.Message)
		case GCStageFailure:
			_, _ = fmt.Fprintf(w, "collector failed: %s\n", e.Error)
		}
	}
}

func drySuffix(dryRun bool) string {
	if dryRun {
		return " — dry-run, nothing will be deleted"
	}
	return ""
}

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect unreferenced registry blobs",
	Long: `Run the stock registry garbage-collect against the shared store,
streaming its output and reporting each stage. Dry-run by default
(preview only): --no-dry-run (or KPR_NO_DRY_RUN=true) collects for
real. The sentinel probes the registry first: readonly collects, a
real run on writable refuses unless --force (flip
storage.maintenance.readonly and restart it instead), a preview on
writable proceeds warned, inconclusive always refuses. After the
collect the sentinel re-probes: a mode flip mid-run is loud but
never a panic — it fails the run unless --force (which presumes you
know). Flipping readonly stays with the operator — this command
never rewrites registry config.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		s, err := OpenStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		out := cmd.OutOrStdout()
		dryRun := !gcNoDryRun && !cfg.NoDryRun
		return runGC(cmd.Context(), out, s, cfg.RegistryURL, gcConfigPath, GCOptions{
			DeleteUntagged: gcDeleteUntagged,
			Force:          gcForce,
			DryRun:         dryRun,
			Report:         renderGCEvent(out, dryRun),
		})
	},
}

func init() {
	gcCmd.Flags().StringVar(&gcConfigPath, "config", "/etc/distribution/config.yml", "Registry config file (shared store paths come from it)")
	gcCmd.Flags().BoolVar(&gcDeleteUntagged, "delete-untagged", false, "Also drop orphaned manifests (same flag as registry garbage-collect)")
	gcCmd.Flags().BoolVar(&gcForce, "force", false, "Collect even when the sentinel finds the registry writable (presumes you know)")
	gcCmd.Flags().BoolVar(&gcNoDryRun, "no-dry-run", false, "Collect for real (default previews with the collector's --dry-run)")
	RootCmd.AddCommand(gcCmd)
}
