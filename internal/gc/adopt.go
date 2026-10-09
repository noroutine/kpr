package gc

import (
	"context"
	"fmt"
	"io"
	"time"

	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// adoptRows is the tracked state adopt prunes on re-pair: old-epoch
// sentinel rows would read as rollback evidence in the next verdict.
// The full store satisfies it; the use case declares only these.
type adoptRows interface {
	All(ctx context.Context) ([]policy.Row, error)
	Delete(ctx context.Context, repo, tag string) error
}

// Adopt pairs the store to the served lineage without minting: the
// explicit ceremony for every verdict that refuses automatic
// pairing. Unpaired stores follow the served identity (or a pinned
// one, which must match); a store paired elsewhere re-pairs and
// prunes the old epoch's sentinel rows; --gen must name the served
// generation, accepting the rollback as baseline. Identity-less
// payloads refuse even here — adopt blesses evidence, not silence.
func Adopt(ctx context.Context, w io.Writer, api sentinel.API, ids lineage.IdentityStore, rows adoptRows, identArg, genArg string) error {
	pay, _, rerr := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	if rerr != nil {
		if !sentinel.Absent(rerr) {
			return fmt.Errorf("sentinel unreadable: %v", rerr)
		}
		if identArg == "" {
			return fmt.Errorf("nothing served: run an armed `kpr store unlock` or `kpr gc` to mint a baseline first (or pass an identity to pre-pair)")
		}
		if err := ids.SetIdentity(ctx, store.Identity{ID: identArg, AdoptedAt: time.Now().UTC()}); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
		_, err := fmt.Fprintf(w, "paired to %s with no baseline: the first mint establishes it\n", identArg)
		return err
	}
	if pay.ID == "" {
		return fmt.Errorf("serves an identity-less generation (pre-pairing): remove the stale tags or wipe the volume, then re-run to establish pairing; never adopted, not even explicitly")
	}
	if identArg != "" && identArg != pay.ID {
		return fmt.Errorf("serves identity %s, you pinned %s: refusing to pair to a stranger (or check the volume mount if unintended)", pay.ID, identArg)
	}
	wantGen := pay.Gen
	if genArg != "" && genArg != pay.Gen {
		return fmt.Errorf("serves generation %s, --gen names %s: the registry moved under you, re-read before accepting", pay.Gen, genArg)
	}
	cur, err := ids.GetIdentity(ctx)
	if err != nil {
		return fmt.Errorf("lineage unreadable: %w", err)
	}
	now := time.Now().UTC()
	switch {
	case cur.ID != "" && cur.ID != pay.ID:
		pruned, err := pruneSentinelRows(ctx, rows)
		if err != nil {
			return fmt.Errorf("old epoch unprunable: %w", err)
		}
		if err := ids.SetIdentity(ctx, store.Identity{ID: pay.ID, BaselineGen: wantGen, AdoptedAt: now}); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
		_, err = fmt.Fprintf(w, "re-paired from %s to %s at generation %s: pruned %d rows of the old epoch\n", cur.ID, pay.ID, wantGen, pruned)
		return err
	case cur.ID != "":
		if err := ids.SetIdentity(ctx, store.Identity{ID: cur.ID, BaselineGen: wantGen, AdoptedAt: cur.AdoptedAt}); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
		_, err = fmt.Fprintf(w, "already paired to %s: baseline refreshed to generation %s\n", cur.ID, wantGen)
		return err
	default:
		if err := ids.SetIdentity(ctx, store.Identity{ID: pay.ID, BaselineGen: wantGen, AdoptedAt: now}); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
		_, err = fmt.Fprintf(w, "paired to %s at generation %s\n", pay.ID, wantGen)
		return err
	}
}

// pruneSentinelRows drops the sentinel repo's rows: they belong to
// the abandoned epoch. Other repos are untouched.
func pruneSentinelRows(ctx context.Context, rows adoptRows) (int, error) {
	all, err := rows.All(ctx)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, r := range all {
		if r.Repo != sentinel.Repo {
			continue
		}
		if err := rows.Delete(ctx, r.Repo, r.Tag); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}
