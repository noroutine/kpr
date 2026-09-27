// Package sweep owns the single-owner delete loop (docs/PLAN.md):
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

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
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

// Sweeper deletes due rows. DryRun plans without touching the registry
// (implicit dry-run: nothing changes unless explicitly armed).
type Sweeper struct {
	Store    store.Store
	Registry *registry.Client
	DryRun   bool
	// Now is a seam for tests; production leaves it nil (wall clock).
	Now func() time.Time
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
func (s *Sweeper) RunPass(ctx context.Context, trigger string) Summary {
	now := s.now()
	sum := Summary{PassID: fmt.Sprintf("%d", now.UnixNano()), Trigger: trigger}

	setStage := func(stage string, due, done int) {
		if err := s.Store.SetCurrent(ctx, store.Current{
			PassID: sum.PassID, Stage: stage, Trigger: trigger,
			StartedAt: now, Due: due, Done: done,
		}); err != nil {
			log.Printf("sweeper: current write failed: %v", err)
		}
	}
	activity := func(r policy.Row, outcome string) {
		if err := s.Store.PushActivity(ctx, store.Outcome{
			Repo: r.Repo, Tag: r.Tag, Reason: r.Reason, Outcome: outcome, At: s.now(),
		}); err != nil {
			log.Printf("sweeper: activity write failed: %v", err)
		}
	}

	held, err := s.Store.AcquireLock(ctx, LockTTL)
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
		if err := s.Store.ReleaseLock(ctx); err != nil {
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
			activity(r, "skipped")
			done++
			continue
		}
		if s.DryRun {
			activity(r, "planned")
			sum.Planned++
			done++
			continue
		}
		outcome, derr := s.Registry.DeleteManifest(ctx, r.Repo, r.Tag)
		switch {
		case derr != nil:
			activity(r, "failed")
			sum.Failed++
			sum.Failures = append(sum.Failures, fmt.Sprintf("%s:%s: %v", r.Repo, r.Tag, derr))
		case outcome == registry.OutcomeHeld:
			activity(r, "untracked")
			sum.Untracked++
			if rerr := s.Store.Delete(ctx, r.Repo, r.Tag); rerr != nil {
				log.Printf("sweeper: row delete failed: %v", rerr)
			}
		default: // deleted or already gone: confirmed, resolve.
			activity(r, "deleted")
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
