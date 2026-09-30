package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
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

// Locker serializes collector runs on one named single-flight lock
// and carries the operator's write intent: store.Store satisfies it
// structurally; the use case declares only the three methods it
// needs. A locked store refuses before anything else — the marker
// is intent, the per-run proof that follows is locality.
type Locker interface {
	AcquireLock(ctx context.Context, name string, ttl time.Duration) (bool, error)
	ReleaseLock(ctx context.Context, name string) error
	IsUnlocked(ctx context.Context) (bool, error)
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
// is the registry's own store with a fresh sentinel generation it
// reads back through the API, then runs the stock collector against
// it under the shared collector lock, streaming every line and
// reporting each stage. The proof needs no tracked rows and no API
// writes: a generation Write lands on the local mount, and only the
// registry serving that same store reads it back. A real (non-dry)
// run on a writable registry refuses: the operator flips
// storage.maintenance.readonly and restarts first. A dry-run preview
// on a writable registry proceeds with a loud warning instead —
// previews delete nothing, so a race only stales the preview.
// Anything unproven — inconclusive probe, unwritten or unreadable
// generation — refuses always. After the collect the sentinel
// re-probes: a mode flip mid-run is loud but never a panic — without
// --force it fails the run (writes may have raced the mark phase),
// with --force the operator presumed to know and the run passes
// warned. A dead post-probe only warns. Flipping readonly stays with
// the operator; this command never rewrites registry config.
func Run(ctx context.Context, w io.Writer, probe Probe, lock Locker, collect Collector, api sentinel.API, registryURL, configPath, binPath string, opts Options) error {
	gcStarted := time.Now()
	unlocked, err := lock.IsUnlocked(ctx)
	if err != nil {
		return fmt.Errorf("store lock unreadable: %w", err)
	}
	if !unlocked {
		return errors.New("store is locked: registry-store writes are denied — run `kpr unlock` to prove the shared store and allow them")
	}
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
	mode, _, err := probe(ctx, registryURL)
	if err != nil {
		return err
	}
	pre := Timed(StagePreProbe, gcStarted)
	pre.Message = ModeName(mode)
	Emit(opts.Report, pre)
	switch mode {
	case ModeWritable, ModeReadonly:
		gen, err := sentinel.NewGen()
		if err != nil {
			return err
		}
		payload := sentinel.Payload{V: 1, Gen: gen, TS: time.Now().UTC().Format(time.RFC3339), Writer: "kpr-gc"}
		if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, payload); err != nil {
			return fmt.Errorf("sentinel generation unwritable under %s: %w", root, err)
		}
		if err := sentinel.Verify(ctx, api, sentinel.Repo, sentinel.Tag, gen); err != nil {
			return fmt.Errorf("kpr does not share this registry's store: %v", err)
		}
		if _, err := fmt.Fprintf(w, "shared store proven via %s:%s generation %s\n", sentinel.Repo, sentinel.Tag, gen); err != nil {
			return err
		}
	default:
		return fmt.Errorf("sentinel inconclusive for %s", registryURL)
	}
	switch mode {
	case ModeWritable:
		if opts.DryRun {
			if _, err := io.WriteString(w, "Warning: registry is writable; dry-run mode, nothing will be deleted\n"); err != nil {
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
		// The generation read-back above is the whole gate: no
		// tracked rows needed, an empty redis proves as well as
		// a full one.
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
	// The verdict goes last: the marking flood buries everything
	// above it, so a preview restates its harmlessness here, where
	// the eye lands.
	if opts.DryRun {
		if _, werr := io.WriteString(w, "dry-run complete: nothing was deleted (collect for real with --no-dry-run)\n"); werr != nil {
			return werr
		}
	}
	return nil
}
