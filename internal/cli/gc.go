package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// registryBinPath is the stock registry binary gc shells out to. The
// image COPYs it from the same registry:3 the stack runs, so collector
// and store versions match by construction.
var registryBinPath = "/bin/registry"

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
	Report         gc.Reporter
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
	if err := gc.Ready(registryBinPath, configPath); err != nil {
		return err
	}
	root, err := gc.StoreRoot(configPath)
	if err != nil {
		return err
	}
	if err := gc.CacheReady(ctx, configPath); err != nil {
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
	pre := gc.Timed(gc.StagePreProbe, gcStarted)
	pre.Message = gc.ModeName(mode)
	gc.Emit(opts.Report, pre)
	switch mode {
	case gc.ModeWritable:
		if !gc.SameStoreUpload(root, uuid) {
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
		row, ok := gc.FirstDigestRow(ctx, s)
		if !ok {
			return errors.New("cannot prove shared store: no tracked digests; push or reap something first, or --force")
		}
		if !gc.SameStoreTagLink(root, row.Repo, row.Tag, row.Digest) {
			return fmt.Errorf("tag %s:%s does not resolve to tracked %s under %s: kpr does not share this registry's store", row.Repo, row.Tag, row.Digest, root)
		}
		if _, err := fmt.Fprintf(w, "shared store proven via %s:%s\n", row.Repo, row.Tag); err != nil {
			return err
		}
	default:
		return fmt.Errorf("sentinel inconclusive for %s", registryURL)
	}
	if err := gc.RunCollector(ctx, w, registryBinPath, gc.Args(configPath, opts.DeleteUntagged, opts.DryRun), opts.Report); err != nil {
		return err
	}
	post, _, perr := gc.ProbeRegistry(ctx, registryURL)
	if perr != nil {
		_, _ = fmt.Fprintf(w, "Warning: post-run probe failed (%v); could not confirm the registry stayed %s\n", perr, gc.ModeName(mode))
		return nil
	}
	pev := gc.Timed(gc.StagePostProbe, gcStarted)
	pev.Message = gc.ModeName(post)
	gc.Emit(opts.Report, pev)
	if post != mode {
		flip := gc.Timed(gc.StageModeFlip, gcStarted)
		flip.Message = gc.ModeName(mode) + "→" + gc.ModeName(post)
		gc.Emit(opts.Report, flip)
		if _, werr := fmt.Fprintf(w, "WARNING: registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run\n", gc.ModeName(mode), gc.ModeName(post)); werr != nil {
			return werr
		}
		if !opts.Force {
			return fmt.Errorf("registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run", gc.ModeName(mode), gc.ModeName(post))
		}
	}
	return nil
}

var gcConfigPath string

var gcDeleteUntagged bool

var gcForce bool

var gcNoDryRun bool

// renderGCEvent voices the lifecycle loud: probe verdicts, collector
// start (pid, so a long mark phase is visibly alive), post-probe, and
// the flip banner. Collector lines stream raw alongside.
func renderGCEvent(w io.Writer, dryRun bool) gc.Reporter {
	return func(e gc.Event) {
		switch e.Stage {
		case gc.StagePreProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry is %s\n", strings.ToUpper(e.Message))
		case gc.StageStarted:
			if e.PID != 0 {
				_, _ = fmt.Fprintf(w, "collector started (pid %d)%s\n", e.PID, drySuffix(dryRun))
			}
		case gc.StagePostProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry still %s\n", strings.ToUpper(e.Message))
		case gc.StageModeFlip:
			_, _ = fmt.Fprintf(w, "WARNING: registry flipped %s mid-run\n", e.Message)
		case gc.StageFailure:
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
