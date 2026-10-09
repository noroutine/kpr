package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// lockTTL bounds a collector run: a crashed gc releases at expiry
// instead of wedging every later run.
// NOTE(mutants): bound arithmetic is equivalent — no test waits
// out 30 minutes to distinguish the bound, and none should.
const lockTTL = 30 * time.Minute

// holdLease bounds the proxy HOLD around an armed collect: a
// crashed gc holds pushes until expiry, so the bound is a
// crash-recovery cost, not a collect budget. It covers lab and
// small registries; collects outrunning it flow unfenced and
// loud (the edge says hold_expired) — a renewing heartbeat is
// the real answer for big ones, and it is future work.
// NOTE(mutants): bound arithmetic is equivalent — no test waits
// out 5 minutes to distinguish the bound, and none should.
const holdLease = 5 * time.Minute

// Probe classifies the registry via the write sentinel: writable,
// readonly, or unknown with the cause.
type Probe func(ctx context.Context, baseURL string) (Mode, string, error)

// probe is the probe seam: the production implementation, swapped
// per test via useSeams. A constant function needs no port —
// nothing varies per run — so the seam carries the substitution
// alone (W13).
var probe Probe = probeRegistry

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

// Recorder tracks minted generations for keep-N: the one method
// minting needs after a verified proof. store.Store satisfies it;
// the use case declares only this.
type Recorder interface {
	Record(ctx context.Context, r policy.Row) error
}

// Options tunes a gc run, and only that: the operator's flags.
// Armed carries the mint — nil previews (the fail-closed
// default), only an explicit --no-dry-run collects for real.
// Everything the run is wired to (reporter, fence) rides Deps,
// never here.
type Options struct {
	DeleteUntagged bool
	Armed          proof.ArmedRun
}

// Accepts groups the sealed risk acceptances beside Options, not
// inside it: evidence is not flags. Named fields, never trailing
// positionals — five same-typed tokens in a row would compile
// swapped and silently misattribute risk. The CLI mints each from
// its --accept-* flag on an armed run (nil otherwise); the online
// preflight consumes the pair it clears, the clock gate, the
// lineage judge, and the post-run flip gate one each.
type Accepts struct {
	Cache     proof.AcceptedRisk
	Fence     proof.AcceptedRisk
	ClockSkew proof.AcceptedRisk
	Rollback  proof.AcceptedRisk
	ModeFlip  proof.AcceptedRisk
}

// Deps carries a run's world beyond its seams: the store in its
// four roles — lock (intent gate plus single-flight), recorder,
// identity, rows — each scriptable apart in tests, one object
// wearing all four hats in production — plus the registry API,
// the clock the mint checks, the event reporter, and the fence.
// Probe and collect stay seams (package vars, W13): constant
// functions carry no per-run variation, so they ride no field. A nil clock derives from
// config.Current() (production never sets it); tests inject
// fakes for the skew and unreachable paths. Config otherwise
// never rides along: Current() names the registry wherever the
// run needs it. The adapter assembles the bundle from its own
// wiring; named fields, never trailing positionals (see Accepts).
// Lifecycle stays outside: opening and closing the store is the
// caller's job.
type Deps struct {
	Lock  Locker
	Rec   Recorder
	Ids   lineage.IdentityStore
	Rows  lineage.Rows
	API   sentinel.API
	Clock clock.Source
	// Report is the event sink. Nil renders to the run's
	// writer via RenderEvent; an injected reporter always wins,
	// so tests observe stages silently.
	Report event.Reporter
	// Store is the whole store behind the split roles above:
	// the fence resolves from its own capability, never from
	// backend-name strings threaded through the call.
	Store fence.GateStore
	// Fence, when non-nil, holds the edge around armed collects
	// (previews never engage). Nil resolves from Store's own
	// capability when a store rides along — an injected fence
	// always wins, so tests drive stubs; nil with no Store stays
	// nil and silent. A fence that fails to engage refuses the
	// run: collecting unfenced when fencing was requested is
	// unknown safety.
	Fence fence.Controller
}

// Run probes the registry writable/readonly, proves the local mount
// is the registry's own store with a fresh sentinel generation it
// reads back through the API, then runs the stock collector against
// it under the shared collector lock, streaming every line and
// reporting each stage. The proof needs no tracked rows and no API
// writes: a generation Write lands on the local mount, and only the
// registry serving that same store reads it back. A serving
// (writable) registry takes the online path: the preflight clears
// the blob cache and the gateway fence (each overridable), armed
// refuses on unaccepted misses, and the collect engages the HOLD
// lease. A stopped (readonly) registry takes the classic offline
// path. A dry-run preview on a writable registry prints the same
// preflight checklist as information and proceeds with a loud
// warning instead — previews delete nothing, so a race only
// stales the preview.
// Anything unproven — inconclusive probe, unwritten or unreadable
// generation — refuses always. After the collect the sentinel
// re-probes: a mode flip mid-run is loud but never a panic — without
// --accept-mode-flip it fails the run (writes may have raced the
// mark phase), with it the operator presumed to know and the run
// passes warned. A dead post-probe only warns. Flipping readonly
// stays with the operator; this command never rewrites registry
// config.
func Run(ctx context.Context, w io.Writer, d Deps, opts Options, accepts Accepts) error {
	gcStarted := time.Now()
	if d.Report == nil {
		d.Report = RenderEvent(w, proof.Unarmed(opts.Armed))
	}
	if d.Fence == nil && d.Store != nil {
		d.Fence = FenceForBackend(d.Store, opts.Armed, w)
	}
	// Intent opens the run: the marker read through the prover, so
	// a locked store refuses with the identical words — only the
	// place guaranteeing them moved. The run consumes the gate;
	// nothing downstream takes the token (re-checking mid-run
	// belongs to the lock-scope decision, Miss 3).
	if _, err := proof.ProveUnlockedStore(ctx, d.Lock); err != nil {
		if errors.Is(err, proof.ErrLocked) {
			return err
		}
		return fmt.Errorf("store lock unreadable: %w", err)
	}
	if err := Ready(config.Current().RegistryBinPath, config.Current().RegistryConfig); err != nil {
		return err
	}
	fsStore, err := proof.ProveFilesystemStore(config.Current().RegistryConfig)
	if err != nil {
		return err
	}
	root := fsStore.Root()
	// Mint timestamps come from a checked clock: skew beyond
	// tolerance refuses unless accepted (--accept-clock-skew); an unreachable NTP warns and
	// proceeds on local time (air-gapped sites stay working). The
	// run consumes the gate — refusals pass through untouched, so
	// every message below reads exactly as before.
	clk := d.Clock
	if clk == nil {
		clk = config.Current().ClockSource()
	}
	if _, cerr := (proof.Checker{Tolerance: clock.Tolerance}.Check(ctx, clk, config.Current().TimeServer)); cerr != nil {
		var skew *clock.SkewError
		if errors.As(cerr, &skew) {
			if accepts.ClockSkew == nil {
				return fmt.Errorf("clock skew %s exceeds %s against %s: fix the clock or re-run with --accept-clock-skew",
					skew.Offset.Round(time.Second), skew.Tolerance, config.Current().TimeServer)
			}
			if _, err := fmt.Fprintf(w, "Warning: clock skew %s exceeds %s; collecting anyway (--accept-clock-skew)\n",
				skew.Offset.Round(time.Second), skew.Tolerance); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(w, "Warning: time source %s unreachable (%v); proceeding with local clock\n", config.Current().TimeServer, cerr); err != nil {
			return err
		}
	}
	held, err := d.Lock.AcquireLock(ctx, store.GCLockKey, lockTTL)
	if err != nil {
		return fmt.Errorf("redis unreachable: %w", err)
	}
	if !held {
		return errors.New("another gc run holds the lock (kpr gc or make gc); wait it out or DEL kpr:gc:lock on the kpr redis DB if stale")
	}
	defer func() {
		if rerr := d.Lock.ReleaseLock(ctx, store.GCLockKey); rerr != nil {
			_, _ = fmt.Fprintf(w, "Warning: gc lock release failed (%v); expires in %v\n", rerr, lockTTL)
		}
	}()
	mode, _, err := probe(ctx, config.Current().RegistryURL)
	if err != nil {
		return err
	}
	pre := event.Timed(StagePreProbe, gcStarted)
	pre.Message = ModeName(mode)
	event.Emit(d.Report, pre)
	// Cleared by the online preflight on the writable path, nil
	// everywhere else: only the writable-armed dispatch consumes
	// them, so a nil here never reaches a delete.
	var onlineCache proof.BlobCacheOff
	var onlineFence proof.GatewayFencingAvailable
	if mode == ModeWritable {
		// The online path: a serving registry collects under the
		// fence, so writability is the mode, not a risk — the
		// risks are a vouched cache and a missing fence, each
		// overridable. The preflight runs before the mint: no
		// point minting a generation the gate will reject. Armed
		// refuses on unaccepted misses; dry-run prints the same
		// checklist as information and previews on.
		cache, fence, report, perr := onlinePreflight(ctx, config.Current().RegistryConfig, config.Current().EdgeAddr, d.Fence != nil, accepts.Cache, accepts.Fence)
		if perr != nil {
			if proof.Unarmed(opts.Armed) {
				if _, err := io.WriteString(w, report+"\n"); err != nil {
					return err
				}
			} else {
				return perr
			}
		} else if _, err := io.WriteString(w, report+"\n"); err != nil {
			return err
		}
		onlineCache, onlineFence = cache, fence
	}
	now := time.Now().UTC()
	switch mode {
	case ModeWritable, ModeReadonly:
		pay, digest, rerr := sentinel.Read(ctx, d.API, sentinel.Repo, sentinel.Tag)
		allRows, err := d.Rows.All(ctx)
		if err != nil {
			return fmt.Errorf("tracked state unreadable: %w", err)
		}
		ident, err := d.Ids.GetIdentity(ctx)
		if err != nil {
			return fmt.Errorf("lineage unreadable: %w", err)
		}
		v := lineage.Judge(
			lineage.Served{Payload: pay, Digest: digest, Err: rerr},
			lineage.Local{Ident: ident, Rows: allRows},
			lineage.Ask{DryRun: proof.Unarmed(opts.Armed), Force: accepts.Rollback != nil, Now: now})
		if !v.Proceed && !v.Establish {
			return fmt.Errorf("%s — %s", v.Reason, v.Action)
		}
		if v.Stale {
			if _, err := fmt.Fprintf(w, "Warning: %s — %s\n", v.Reason, v.Action); err != nil {
				return err
			}
		}
		if proof.Unarmed(opts.Armed) {
			break
		}
		useID := ident.ID
		if v.Establish {
			est := store.Identity{ID: useID, BaselineGen: ident.BaselineGen, AdoptedAt: now}
			if useID == "" {
				newID, err := sentinel.NewGen()
				if err != nil {
					return err
				}
				useID = newID
				est.ID = useID
			} else {
				// Re-minting, not re-pairing: the acceptance the
				// operator recorded (baseline, adopted-at) survives.
				est.AdoptedAt = ident.AdoptedAt
				if _, err := fmt.Fprintf(w, "Warning: store paired to %s but nothing served — re-minting the baseline (check the mount if unintended)\n", useID); err != nil {
					return err
				}
			}
			if err := d.Ids.SetIdentity(ctx, est); err != nil {
				return fmt.Errorf("lineage unrecordable: %w", err)
			}
		}
		gen, err := sentinel.NewGen()
		if err != nil {
			return err
		}
		payload := sentinel.Payload{V: 1, Gen: gen, ID: useID, TS: now.Format(time.RFC3339), Writer: "kpr-gc"}
		md, err := writeVerifiedGeneration(ctx, d.API, root, payload)
		if err != nil {
			return err
		}
		if err := d.Rec.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: gen, Digest: md, MediaType: sentinel.ManifestMediaType, PushedAt: now, Actor: payload.Writer}); err != nil {
			return fmt.Errorf("proof held but the generation went untracked: %w", err)
		}
		if v.Heal != nil {
			if err := d.Rec.Record(ctx, *v.Heal); err != nil {
				return fmt.Errorf("adopted generation went untracked: %w", err)
			}
		}
		if _, err := fmt.Fprintf(w, "shared store proven via %s:%s generation %s\n", sentinel.Repo, sentinel.Tag, gen); err != nil {
			return err
		}
	default:
		return fmt.Errorf("sentinel inconclusive for %s", config.Current().RegistryURL)
	}
	switch mode {
	case ModeWritable:
		// Uncleared armed runs refused at the preflight, before the
		// proof: what reaches here is a preview or a cleared online
		// run (proven or per-risk accepted).
		if proof.Unarmed(opts.Armed) {
			if _, err := io.WriteString(w, "Warning: registry is writable; dry-run mode, nothing will be deleted\n"); err != nil {
				return err
			}
		} else {
			if _, err := io.WriteString(w, "Warning: registry is writable; collecting under the cleared online preflight\n"); err != nil {
				return err
			}
		}
	case ModeReadonly:
		// The generation read-back above is the whole gate: no
		// tracked rows needed, an empty redis proves as well as
		// a full one.
	}
	// The dispatch, not a flag, decides which collect runs: previews
	// take the bare collect (nothing proven, nothing needed), armed
	// readonly the same (the mint above is the whole gate), and only
	// the writable-armed path carries the extra demand — which is why
	// only it has a variant.
	// fenced runs an armed collect under the proxy HOLD lease:
	// engage, collect, release. No fence configured means no fence
	// (previews never reach here); an armed run reaches here
	// fenceless only through an explicit --accept-unfenced — the
	// preflight refused everything else. A fence that fails to
	// engage refuses instead of collecting unfenced. The lease
	// outlives a crashed collect by design — expiry, not release,
	// bounds it.
	fenced := func(op func() error) error {
		if d.Fence == nil {
			return op()
		}
		release, err := d.Fence.Hold(ctx, time.Now().Add(holdLease))
		if err != nil {
			return fmt.Errorf("gc: engage proxy fence: %w", err)
		}
		// The run voices its own fence lines: the adapter
		// records to the ring, narration into this stream
		// belongs here, at the moments this function owns.
		event.Emit(d.Report, event.Event{Stage: StageHoldEngage, Message: "HOLD lease engaged: manifest writes wait out the armed collect"})
		defer func() {
			release()
			event.Emit(d.Report, event.Event{Stage: StageHoldRelease, Message: "HOLD lease released: manifest writes flow again"})
		}()
		return op()
	}

	switch {
	case proof.Unarmed(opts.Armed):
		if err := collect(ctx, w, config.Current().RegistryBinPath, Args(config.Current().RegistryConfig, opts.DeleteUntagged, true), d.Report); err != nil {
			return err
		}
	case mode == ModeWritable:
		if err := fenced(func() error {
			return collectWritableArmed(ctx, w, collect, config.Current().RegistryBinPath, Args(config.Current().RegistryConfig, opts.DeleteUntagged, false), d.Report, onlineCache, onlineFence)
		}); err != nil {
			return err
		}
	default:
		if err := fenced(func() error {
			return collect(ctx, w, config.Current().RegistryBinPath, Args(config.Current().RegistryConfig, opts.DeleteUntagged, false), d.Report)
		}); err != nil {
			return err
		}
	}
	// The collector deletes blobs and links but leaves their
	// parents: an armed run removes tagless repo husks first,
	// then prunes the empty skeleton both left behind (husk
	// removal empties namespace parents prune cleans). Both
	// walks run silent for a while on big roots, so each
	// narrates its start in the present tense and its count
	// after the fact — the past-tense verdict never drops from
	// nowhere. Previews delete nothing, so they remove nothing
	// either. A husk or prune failure warns, never fails: the
	// collection already succeeded, and occupancy races resolve
	// safe — anything else (permissions, I/O) names itself in
	// the warning.
	if !proof.Unarmed(opts.Armed) {
		if _, werr := fmt.Fprintf(w, "pruning husks...\n"); werr != nil {
			return werr
		}
		husked, herr := RemoveHusks(root)
		hev := event.Timed(StageHusk, gcStarted)
		if herr != nil {
			hev.Error = herr.Error()
			event.Emit(d.Report, hev)
			if _, werr := fmt.Fprintf(w, "Warning: husk cleanup incomplete (%v)\n", herr); werr != nil {
				return werr
			}
		} else {
			hev.Message = fmt.Sprintf("%d repos", len(husked))
			event.Emit(d.Report, hev)
			if len(husked) > 0 {
				if _, werr := fmt.Fprintf(w, "pruned %d husks\n", len(husked)); werr != nil {
					return werr
				}
			}
		}
		if _, werr := fmt.Fprintf(w, "pruning empty directories...\n"); werr != nil {
			return werr
		}
		pruned, perr := PruneEmptyDirs(root)
		pev := event.Timed(StagePrune, gcStarted)
		if perr != nil {
			pev.Error = perr.Error()
			event.Emit(d.Report, pev)
			if _, werr := fmt.Fprintf(w, "Warning: empty-dir cleanup incomplete (%v)\n", perr); werr != nil {
				return werr
			}
		} else {
			pev.Message = fmt.Sprintf("%d dirs", pruned)
			event.Emit(d.Report, pev)
			if _, werr := fmt.Fprintf(w, "pruned %d empty directories\n", pruned); werr != nil {
				return werr
			}
		}
	}
	post, _, perr := probe(ctx, config.Current().RegistryURL)
	if perr != nil {
		_, _ = fmt.Fprintf(w, "Warning: post-run probe failed (%v); could not confirm the registry stayed %s\n", perr, ModeName(mode))
		return nil
	}
	pev := event.Timed(StagePostProbe, gcStarted)
	pev.Message = ModeName(post)
	event.Emit(d.Report, pev)
	if post != mode {
		flip := event.Timed(StageModeFlip, gcStarted)
		flip.Message = ModeName(mode) + "→" + ModeName(post)
		event.Emit(d.Report, flip)
		if _, werr := fmt.Fprintf(w, "WARNING: registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run\n", ModeName(mode), ModeName(post)); werr != nil {
			return werr
		}
		if accepts.ModeFlip == nil {
			return fmt.Errorf("registry mode changed during collection (%s→%s): writes may have raced the mark phase; verify pulls before trusting this run, or re-run with --accept-mode-flip", ModeName(mode), ModeName(post))
		}
	}
	// The verdict goes last: the marking flood buries everything
	// above it, so a preview restates its harmlessness here, where
	// the eye lands.
	if proof.Unarmed(opts.Armed) {
		if _, werr := io.WriteString(w, "dry-run complete: nothing was deleted (collect for real with --no-dry-run)\n"); werr != nil {
			return werr
		}
	}
	return nil
}
