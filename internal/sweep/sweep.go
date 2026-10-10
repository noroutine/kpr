// Package sweep owns the single-owner delete loop (docs/ARCHITECTURE.md):
// the sweeper in `serve` is the only writer that deletes from the
// registry. A pass reads due rows, enforces the TTL expiry floor
// regardless of the mark, and resolves every row exactly once —
// deleted, gone, untracked, planned (dry-run), failed, or skipped.
package sweep

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"nrtn.dev/catalyst/kpr/internal/otel"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Fixed stage vocabulary: one pass, no job records, nothing to retry
// except asking again for rows that are still due.
const (
	StageStart   = "start"
	StageSkip    = "skip"
	StageRow     = "row"
	StageDone    = "done"
	StageFailure = "failure"
)

// LockTTL bounds single-flight: a crashed sweeper can't hold it forever.
// NOTE(mutants): minute steps are unobservable — no test waits out
// 5 minutes, and none should. Collapse to zero is refused by the
// positivity pin below: redis hands the bound to PX as-is, and a
// zero TTL locks nothing (file and mem ignore it, so only the pin
// sees the collapse).
const LockTTL = 5 * time.Minute

// Summary is the pass outcome: RunPass returns it, so `sweep` gets
// synchronous feedback without polling or an HTTP round-trip.
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

// Sweeper deletes due rows. Unarmed it plans without touching the
// registry (implicit dry-run: nothing changes unless explicitly
// armed). The sweeper proves nothing — callers pass the proof.
type Sweeper struct {
	Store    store.Store
	Registry Registry
	// Sentinel reads the served lineage: a pass over a foreign,
	// silent, or rolled-back registry refuses before the lock, let
	// alone any delete. The sweeper never mints, so it cannot
	// establish — silence refuses in both modes.
	Sentinel sentinel.API
	Armed    proof.ArmedRun
	// Now is a seam for tests; production leaves it nil (wall clock).
	Now func() time.Time
	// Log emits activity records: one per resolved row plus a pass
	// summary. Nil defaults to the process OTel logger — plain stdout
	// text when telemetry is off, stdout plus Quickwit via OTLP when
	// the observability overlay enables it.
	Log func(ctx context.Context, msg string, args ...any)
	// Progress reports the running summary after every settled
	// row: the CLI repaints one live line off it. Nil skips it.
	Progress func(Summary)
	// RowLog takes the per-row stream (one line per verdict:
	// would sweep/swept with the digest, would skip/skipped with
	// the reason, untracked; failures already stream as `failed:`
	// lines through the caller, so they are not repeated here);
	// nil discards it. Set for --output logs, never for stdout —
	// the terminal keeps the live line, the ring keeps every
	// verdict.
	RowLog io.Writer
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

	// Every exit narrates its summary: a skip in Quickwit must read
	// as "nothing due", not as "sweeper went quiet".
	defer func() {
		args := []any{
			// The key stays dry_run: dashboards read it, and
			// observability owns its vocabulary (see docs/DRY_RUN.md).
			"pass_id", sum.PassID, "trigger", trigger, "dry_run", proof.Unarmed(s.Armed),
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

	// Intent, then lineage, then lock: no point reading generations
	// or holding single-flight for a pass that must not act, and a
	// refused pass must not resolve (let alone delete) anything.
	// Locked refuses runs outright, previews included (a pass is a
	// run, not a read). The sweeper carries no accept flags: stale armed
	// refuses; stale dry-run proceeds to plan, which deletes nothing.
	// Heal rows are the mint path's job (gc adopt-records); the sweep
	// acts on due rows only.
	//
	// Shared preambles, two consumers each: the pass consumes the
	// gates — proceed or refuse, narrated below — while directed
	// deletes take the tokens themselves to check staleness.
	if s.Sentinel == nil {
		setStage(StageFailure, 0, 0)
		sum.Skipped = true
		sum.Failures = append(sum.Failures, "sweeper miswired: no sentinel reader (refusing instead of sweeping blind)")
		return sum
	}
	if _, err := proof.ProveUnlockedStore(ctx, s.Store); err != nil {
		setStage(StageFailure, 0, 0)
		sum.Skipped = true
		sum.Failures = append(sum.Failures, err.Error())
		return sum
	}
	// One frozen clock for gate and pass alike: the verdict judges
	// as of pass start, not as of however long the reads took.
	if _, err := (proof.Prover{Sentinel: s.Sentinel, Store: s.Store, DryRun: proof.Unarmed(s.Armed), Now: func() time.Time { return now }}.Prove(ctx)); err != nil {
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
	progress := func() {
		if s.Progress != nil {
			s.Progress(sum)
		}
	}
	progress()

	done := 0
	// skipVerb narrates holds in the pass's own tense: armed
	// skips what is, dry-run previews what would be.
	skipVerb := "skipped"
	if proof.Unarmed(s.Armed) {
		skipVerb = "would skip"
	}
	for _, r := range due {
		setStage(StageRow, len(due), done)
		// Expiry floor: a TTL row whose promise hasn't elapsed is
		// held no matter what the mark says (a stale mark must not
		// wipe a fresh push).
		if _, isTTL := policy.EffectiveTTL(r.Tag); isTTL && !policy.Eligible(r.Tag, r.PushedAt, now) {
			resolve(r, "skipped", nil)
			s.logRow(sweepLine(skipVerb, r, "ttl not elapsed"))
			done++
			progress()
			continue
		}
		// Pre-delete re-read: the pass holds pass-start state, but a
		// push may have cleared the mark (or untracked the row)
		// since. The armed run deletes only what is still due
		// under an unchanged digest; anything else skips for the
		// next pass to re-evaluate. Dry-run previews through the
		// same gate, so the plan promises what arming performs.
		// This narrows the mark-to-delete race to the check-delete
		// instant; it does not close it (no lock binds the push
		// path — fencing or epoch-CAS would; the freshest state
		// read here is stale again by the delete).
		cur, ok, gerr := s.Store.Get(ctx, r.Repo, r.Tag)
		if gerr != nil {
			sum.Failures = append(sum.Failures, fmt.Sprintf("%s:%s: %v", r.Repo, r.Tag, gerr))
			resolve(r, "failed", gerr)
			sum.Failed++
			done++
			progress()
			continue
		}
		switch {
		case !ok:
			resolve(r, "skipped", nil)
			s.logRow(sweepLine(skipVerb, r, "untracked mid-pass"))
			done++
			progress()
			continue
		case !cur.Due:
			resolve(r, "skipped", nil)
			s.logRow(sweepLine(skipVerb, r, "mark cleared"))
			done++
			progress()
			continue
		case cur.Digest != r.Digest:
			resolve(r, "skipped", nil)
			s.logRow(sweepLine(skipVerb, r, "digest moved"))
			done++
			progress()
			continue
		}
		// Through the gate: the pass takes the row on in both
		// modes, so it counts as planned before resolving as
		// performed, failed, or untracked (armed) or performed
		// without deleting (dry-run).
		sum.Planned++
		if proof.Unarmed(s.Armed) {
			resolve(r, "planned", nil)
			// Dry-run performs everything short of the delete: the
			// row passed the same gate arming would enforce, so it
			// counts as performed too. Read-path failures above
			// already count as failed; registry-delete failures
			// are undetectable without deleting (armed-only).
			sum.Performed++
			s.logRow(sweepLine("would sweep", r, ""))
			done++
			progress()
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
			s.logRow(sweepLine("untracked", r, "registry held"))
			if rerr := s.Store.Delete(ctx, r.Repo, r.Tag); rerr != nil {
				log.Printf("sweeper: row delete failed: %v", rerr)
			}
		default: // deleted or already gone: confirmed, resolve.
			resolve(r, "deleted", nil)
			sum.Performed++
			s.logRow(sweepLine("swept", r, ""))
			if rerr := s.Store.Delete(ctx, r.Repo, r.Tag); rerr != nil {
				log.Printf("sweeper: row delete failed: %v", rerr)
			}
		}
		done++
		progress()
	}
	setStage(StageDone, len(due), done)
	return sum
}

// sweepLine formats one per-row verdict for the RowLog stream:
// "would sweep repo:tag digest", "swept repo:tag digest",
// "would skip repo:tag (reason)". The digest trails only when
// the row carries one (digest-less rows delete by tag).
func sweepLine(verb string, r policy.Row, reason string) string {
	var b strings.Builder
	b.WriteString(verb)
	b.WriteString(" " + r.Repo + ":" + r.Tag)
	if r.Digest != "" {
		b.WriteString(" " + r.Digest)
	}
	if reason != "" {
		b.WriteString(" (" + reason + ")")
	}
	return b.String()
}

// logRow appends one verdict line to the RowLog stream, if set.
// A failing stream must not fail the pass (the ring already holds
// every verdict); it logs, like the rest of the event path.
func (s *Sweeper) logRow(line string) {
	if s.RowLog == nil {
		return
	}
	if _, err := fmt.Fprintln(s.RowLog, line); err != nil {
		log.Printf("sweeper: row log write failed: %v", err)
	}
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
