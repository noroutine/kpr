package sweep

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// resolveRow records one outcome in the redis ring and on the
// activity log together: the console and Quickwit never diverge.
// Shared by the pass loop and directed deletes. Every record signs
// the sweeper and the trigger that wrote it — a pass and an untag
// journal the same outcomes, and only the trigger tells them apart.
func (s *Sweeper) resolveRow(ctx context.Context, passID, trigger string, r policy.Row, outcome string, rerr error) {
	reason := r.Reason
	if reason == "" {
		// Operator-directed deletes go around the plan, so their
		// rows carry no due reason: the ring names the cause — the
		// trigger, or the error on failure — instead of ().
		reason = trigger
		if rerr != nil {
			reason = rerr.Error()
		}
	}
	if err := s.Store.PushActivity(ctx, store.Outcome{
		Repo: r.Repo, Tag: r.Tag, Reason: reason, Outcome: outcome, At: s.now(),
		Actor: "kpr-sweep", Trigger: trigger,
	}); err != nil {
		log.Printf("sweeper: activity write failed: %v", err)
	}
	args := []any{
		"pass_id", passID,
		"repo", r.Repo, "tag", r.Tag, "reason", reason, "outcome", outcome,
	}
	if rerr != nil {
		args = append(args, "err", rerr.Error())
	}
	s.emit(ctx, "sweep row", args...)
}

// deleteManifest deletes one row's manifest by digest: modern
// registries (distribution:3) reject tag deletes outright, while a
// digest delete is confirmed and universal. Digest-less rows fall
// back to the tag and fail visibly where unsupported.
func (s *Sweeper) deleteManifest(ctx context.Context, r policy.Row) (string, error) {
	ref := r.Digest
	if ref == "" {
		ref = r.Tag
	}
	return s.Registry.DeleteManifest(ctx, r.Repo, ref)
}

// Untrack drops the given rows without touching the registry:
// bare `rm`. The tags survive untracked (until a re-push or
// backfill re-tracks them); the outcome journals as untracked,
// the same meaning the pass loop records when the registry holds
// a delete. A row-drop failure continues the batch and fails
// loudly in aggregate — like Untag, the confirmed rows print
// before the error returns.
func (s *Sweeper) Untrack(ctx context.Context, rows []policy.Row, unlocked proof.UnlockedStore) ([]policy.Row, error) {
	if unlocked == nil {
		return nil, errors.New("untrack on a locked store: refusing to drop rows blind (run `kpr store unlock` first)")
	}
	id := fmt.Sprintf("%d", s.now().UnixNano())
	var done []policy.Row
	var failed []string
	for _, r := range rows {
		if err := s.Store.Delete(ctx, r.Repo, r.Tag); err != nil {
			failed = append(failed, fmt.Sprintf("%s:%s: %v", r.Repo, r.Tag, err))
			continue
		}
		s.resolveRow(ctx, id, "rm", r, "untracked", nil)
		done = append(done, r)
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("untrack failed for %d (%s)", len(failed), strings.Join(failed, "; "))
	}
	return done, nil
}

// Untag deletes the given rows' manifests without requiring due
// marks: the operator-directed delete (`store rm --untag`), a
// specific sweep going around the plan. The caller proves the
// shared store first and passes the token — nil refuses (deleting
// blind), stale refuses (the served view lags tracked state, so a
// delete would land against a moved registry). Intent rides the
// second token: locked refuses before identity is even asked.
// Each row drops only on confirm (deleted or already gone); held
// and failed deletes keep their rows and fail loudly in aggregate.
func (s *Sweeper) Untag(ctx context.Context, rows []policy.Row, same proof.SameStore, unlocked proof.UnlockedStore) ([]policy.Row, error) {
	if unlocked == nil {
		return nil, errors.New("untag on a locked store: refusing to delete blind (run `kpr store unlock` first)")
	}
	if same == nil {
		return nil, errors.New("untag without same-store proof: refusing to delete blind (prove the shared store first)")
	}
	if same.Stale() {
		return nil, fmt.Errorf("untag on stale proof (served %s lags tracked state): re-read before deleting", same.Generation())
	}
	id := fmt.Sprintf("%d", s.now().UnixNano())
	var done []policy.Row
	var failed []string
	for _, r := range rows {
		outcome, derr := s.deleteManifest(ctx, r)
		switch {
		case derr != nil:
			s.resolveRow(ctx, id, "untag", r, "failed", derr)
			failed = append(failed, fmt.Sprintf("%s:%s: %v", r.Repo, r.Tag, derr))
		case outcome == registry.OutcomeHeld:
			herr := errors.New("registry held the delete")
			s.resolveRow(ctx, id, "untag", r, "failed", herr)
			failed = append(failed, fmt.Sprintf("%s:%s: %v", r.Repo, r.Tag, herr))
		default: // deleted or already gone: confirmed, resolve.
			s.resolveRow(ctx, id, "untag", r, "deleted", nil)
			if err := s.Store.Delete(ctx, r.Repo, r.Tag); err != nil {
				// Like the pass loop: a local store failure must not
				// abandon the batch — the manifest is already gone, a
				// re-untag converges on `gone`.
				log.Printf("sweeper: untag row delete failed: %v", err)
				failed = append(failed, fmt.Sprintf("%s:%s: row drop failed: %v", r.Repo, r.Tag, err))
				continue
			}
			done = append(done, r)
		}
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("untag failed for %d (%s): rows kept", len(failed), strings.Join(failed, "; "))
	}
	return done, nil
}
