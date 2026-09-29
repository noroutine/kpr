package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// lockTTL bounds a collector run: a crashed gc releases at expiry
// instead of wedging every later run.
const lockTTL = 30 * time.Minute

// Probe classifies the registry via the write sentinel: writable,
// readonly, or unknown with the cause. ProbeRegistry is the
// production implementation; tests substitute a stub. Consumed by
// Run below.
type Probe func(ctx context.Context, baseURL string) (Mode, string, error)

// ProbeRegistry satisfies Probe: the assertion pins the port to the
// implementation it carries.
var _ Probe = ProbeRegistry

// Locker serializes collector runs on one named single-flight lock.
// store.Store satisfies it structurally; the use case declares only
// the two methods it needs.
type Locker interface {
	AcquireLock(ctx context.Context, name string, ttl time.Duration) (bool, error)
	ReleaseLock(ctx context.Context, name string) error
}

// Options tunes a gc run: the operator's flags plus the event
// reporter the CLI renders loud. DryRun previews (the default) —
// only an explicit --no-dry-run collects for real.
type Options struct {
	DeleteUntagged bool
	Force          bool
	DryRun         bool
	Report         Reporter
}

// Run probes the registry writable/readonly, proves the local mount
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
func Run(ctx context.Context, w io.Writer, s store.Store, probe Probe, lock Locker, collect Collector, registryURL, configPath, binPath string, opts Options) error {
	gcStarted := time.Now()
	if err := Ready(binPath, configPath); err != nil {
		return err
	}
	root, err := StoreRoot(configPath)
	if err != nil {
		return err
	}
	if err := CacheReady(ctx, configPath); err != nil {
		return err
	}
	held, err := lock.AcquireLock(ctx, store.GCLockKey, lockTTL)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	if !held {
		return errors.New("another gc run holds the lock (kpr gc or make gc); wait it out or DEL kpr:gc:lock on the kpr redis DB if stale")
	}
	defer func() {
		if rerr := lock.ReleaseLock(ctx, store.GCLockKey); rerr != nil {
			_, _ = fmt.Fprintf(w, "Warning: gc lock release failed (%v); expires in %v\n", rerr, lockTTL)
		}
	}()
	mode, uuid, err := probe(ctx, registryURL)
	if err != nil {
		return err
	}
	pre := Timed(StagePreProbe, gcStarted)
	pre.Message = ModeName(mode)
	Emit(opts.Report, pre)
	switch mode {
	case ModeWritable:
		if !SameStoreUpload(root, uuid) {
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
	case ModeReadonly:
		row, ok := FirstDigestRow(ctx, s)
		if !ok {
			return errors.New("cannot prove shared store: no tracked digests; push or reap something first, or --force")
		}
		if !SameStoreTagLink(root, row.Repo, row.Tag, row.Digest) {
			return fmt.Errorf("tag %s:%s does not resolve to tracked %s under %s: kpr does not share this registry's store", row.Repo, row.Tag, row.Digest, root)
		}
		if _, err := fmt.Fprintf(w, "shared store proven via %s:%s\n", row.Repo, row.Tag); err != nil {
			return err
		}
	default:
		return fmt.Errorf("sentinel inconclusive for %s", registryURL)
	}
	if err := collect(ctx, w, binPath, Args(configPath, opts.DeleteUntagged, opts.DryRun), opts.Report); err != nil {
		return err
	}
	post, _, perr := probe(ctx, registryURL)
	if perr != nil {
		_, _ = fmt.Fprintf(w, "Warning: post-run probe failed (%v); could not confirm the registry stayed %s\n", perr, ModeName(mode))
		return nil
	}
	pev := Timed(StagePostProbe, gcStarted)
	pev.Message = ModeName(post)
	Emit(opts.Report, pev)
	if post != mode {
		flip := Timed(StageModeFlip, gcStarted)
		flip.Message = ModeName(mode) + "→" + ModeName(post)
		Emit(opts.Report, flip)
		if _, werr := fmt.Fprintf(w, "WARNING: registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run\n", ModeName(mode), ModeName(post)); werr != nil {
			return werr
		}
		if !opts.Force {
			return fmt.Errorf("registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run", ModeName(mode), ModeName(post))
		}
	}
	return nil
}
