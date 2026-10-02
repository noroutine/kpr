// Package sweep owns the single-owner delete loop (docs/ARCHITECTURE.md):
// the sweeper in `serve` is the only writer that deletes from the
// registry. A pass reads due rows, enforces the TTL expiry floor
// regardless of the mark, and resolves every row exactly once —
// deleted, gone, untracked, planned (dry-run), failed, or skipped.
package sweep

import (
	"context"
	"fmt"
	"log"
	"time"

	"nrtn.dev/catalyst/kpr/internal/otel"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Fixed stage vocabulary: one pass, no job records, nothing to retry
// except the next tick picking up rows that are still due.
const (
	StageStart   = "start"
	StageSkip    = "skip"
	StageRow     = "row"
	StageDone    = "done"
	StageFailure = "failure"
)

// LockTTL bounds single-flight: a crashed sweeper can't hold it forever.
const LockTTL = 5 * time.Minute

// TickInterval is the sweeper tick: marked rows are picked up on the
// next tick even if nobody ever POSTs the trigger (the trigger is an
// accelerator, not a dependency).
const TickInterval = time.Minute

// Summary is the pass outcome: the sweep endpoint's HTTP response
// carries it, so `sweep` gets synchronous feedback without polling.
type Summary struct {
	PassID    string
	Trigger   string
	Skipped   bool
	Performed int
	Planned   int
	Failed    int
	Untracked int
	Failures  []string
}

// Registry is the outbound port the sweep use case consumes: delete
// by repo and reference. *registry.Client is the production adapter;
// tests bring a stub, never a loopback server.
type Registry interface {
	DeleteManifest(ctx context.Context, repo, ref string) (string, error)
}

// Sweeper deletes due rows. DryRun plans without touching the registry
// (implicit dry-run: nothing changes unless explicitly armed).
type Sweeper struct {
	Store    store.Store
	Registry Registry
	// Sentinel reads the served lineage: a pass over a foreign,
	// silent, or rolled-back registry refuses before the lock, let
	// alone any delete. The sweeper never mints, so it cannot
	// establish — silence refuses in both modes.
	Sentinel sentinel.API
	DryRun   bool
	// Now is a seam for tests; production leaves it nil (wall clock).
	Now func() time.Time
	// Log emits activity records: one per resolved row plus a pass
	// summary. Nil defaults to the process OTel logger — plain stdout
	// text when telemetry is off, stdout plus Quickwit via OTLP when
	// the observability overlay enables it.
	Log func(ctx context.Context, msg string, args ...any)
}

func (s *Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// RunPass runs one sweep to its summary. Store write errors on the
// event path (current/activity) are logged, never fatal; a pass fails
// only when it cannot read what is due.
func (s *Sweeper) RunPass(ctx context.Context, trigger string) (sum Summary) {
	now := s.now()
	sum = Summary{PassID: fmt.Sprintf("%d", now.UnixNano()), Trigger: trigger}

	// Every exit narrates its summary: a skipped tick in Quickwit
	// must read as "nothing due", not as "sweeper went quiet".
	defer func() {
		args := []any{
			"pass_id", sum.PassID, "trigger", trigger, "dry_run", s.DryRun,
			"performed", sum.Performed, "planned", sum.Planned,
			"failed", sum.Failed, "untracked", sum.Untracked,
			"skipped", sum.Skipped,
		}
		if len(sum.Failures) > 0 {
			args = append(args, "failures", sum.Failures)
		}
		s.emit(ctx, "sweep pass", args...)
	}()

	setStage := func(stage string, due, done int) {
		if err := s.Store.SetCurrent(ctx, store.Current{
			PassID: sum.PassID, Stage: stage, Trigger: trigger,
			StartedAt: now, Due: due, Done: done,
		}); err != nil {
			log.Printf("sweeper: current write failed: %v", err)
		}
	}
	// resolve records the outcome in the redis ring and on the
	// activity log together: the console and Quickwit never diverge.
	resolve := func(r policy.Row, outcome string, rerr error) {
		s.resolveRow(ctx, sum.PassID, trigger, r, outcome, rerr)
	}

	// Lineage before lock: no point holding single-flight for a
	// pass that must not act, and a refused pass must not resolve
	// (let alone delete) anything. The sweeper carries no --force:
	// stale armed refuses; stale dry-run proceeds to plan, which
	// deletes nothing. Heal rows are the mint path's job (gc
	// adopt-records); the sweep acts on due rows only.
	//
	// One preamble (Prover), two consumers: the pass consumes the
	// gate — proceed or refuse, narrated below — while directed
	// deletes (Untag) take the token itself to check staleness.
	if s.Sentinel == nil {
		setStage(StageFailure, 0, 0)
		sum.Skipped = true
		sum.Failures = append(sum.Failures, "sweeper miswired: no sentinel reader (refusing instead of sweeping blind)")
		return sum
	}
	// One frozen clock for gate and pass alike: the verdict judges
	// as of pass start, not as of however long the reads took.
	if _, err := (proof.Prover{Sentinel: s.Sentinel, Store: s.Store, DryRun: s.DryRun, Now: func() time.Time { return now }}.Prove(ctx)); err != nil {
		setStage(StageFailure, 0, 0)
		sum.Skipped = true
		sum.Failures = append(sum.Failures, err.Error())
		return sum
	}

	held, err := s.Store.AcquireLock(ctx, store.LockKey, LockTTL)
	if err != nil {
		setStage(StageFailure, 0, 0)
		sum.Failures = append(sum.Failures, fmt.Sprintf("lock: %v", err))
		return sum
	}
	if !held {
		setStage(StageSkip, 0, 0)
		sum.Skipped = true
		return sum
	}
	defer func() {
		if err := s.Store.ReleaseLock(ctx, store.LockKey); err != nil {
			log.Printf("sweeper: lock release failed: %v", err)
		}
	}()

	due, err := s.Store.Due(ctx)
	if err != nil {
		setStage(StageFailure, 0, 0)
		sum.Failures = append(sum.Failures, fmt.Sprintf("read due: %v", err))
		return sum
	}
	if len(due) == 0 {
		setStage(StageSkip, 0, 0)
		sum.Skipped = true
		return sum
	}
	setStage(StageStart, len(due), 0)

	done := 0
	for _, r := range due {
		setStage(StageRow, len(due), done)
		// Expiry floor: a TTL row whose promise hasn't elapsed is
		// held no matter what the mark says (a stale mark must not
		// wipe a fresh push).
		if _, isTTL := policy.EffectiveTTL(r.Tag); isTTL && !policy.Eligible(r.Tag, r.PushedAt, now) {
			resolve(r, "skipped", nil)
			done++
			continue
		}
		if s.DryRun {
			resolve(r, "planned", nil)
			sum.Planned++
			done++
			continue
		}
		outcome, derr := s.deleteManifest(ctx, r)
		switch {
		case derr != nil:
			sum.Failures = append(sum.Failures, fmt.Sprintf("%s:%s: %v", r.Repo, r.Tag, derr))
			resolve(r, "failed", derr)
			sum.Failed++
		case outcome == registry.OutcomeHeld:
			resolve(r, "untracked", nil)
			sum.Untracked++
			if rerr := s.Store.Delete(ctx, r.Repo, r.Tag); rerr != nil {
				log.Printf("sweeper: row delete failed: %v", rerr)
			}
		default: // deleted or already gone: confirmed, resolve.
			resolve(r, "deleted", nil)
			sum.Performed++
			if rerr := s.Store.Delete(ctx, r.Repo, r.Tag); rerr != nil {
				log.Printf("sweeper: row delete failed: %v", rerr)
			}
		}
		done++
	}
	setStage(StageDone, len(due), done)
	return sum
}

// emit sends one activity record to the injected Log sink, or to the
// process OTel logger when none is set (resolved lazily so a serve
// that Init's telemetry after construction still fans out to OTLP).
func (s *Sweeper) emit(ctx context.Context, msg string, args ...any) {
	if s.Log != nil {
		s.Log(ctx, msg, args...)
		return
	}
	otel.Logger().InfoContext(ctx, msg, args...)
}
