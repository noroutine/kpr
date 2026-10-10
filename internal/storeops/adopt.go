package storeops

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"nrtn.dev/catalyst/kpr/internal/helpers/words"
	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
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

// adoptRegistry is the catalog half the re-pair's untag walk needs:
// list the machinery namespace, read back per-repo tags, delete
// manifests by tag. *registry.Client satisfies it; the walk stays
// driver-agnostic (HTTP deletes, no fs/s3 surgery), so the epoch
// cut works on any storage backend.
type adoptRegistry interface {
	CatalogAll(ctx context.Context) ([]string, error)
	Catalog(ctx context.Context, repo string) ([]string, error)
	DeleteManifest(ctx context.Context, repo, ref string) (string, error)
}

// sentinelPrefix bounds the untag walk: our sentinels live here.
// Mirrors backfill's SentinelPrefix; the ceremony and the backfill
// walk agree on the machinery namespace.
const sentinelPrefix = "noroutine/kpr-"

// Adopt pairs the store to the served lineage without minting: the
// explicit ceremony for every verdict that refuses automatic
// pairing. Dry-run previews every branch (the absence of Armed is
// the preview); armed performs. Unpaired stores follow the served
// identity (or a pinned one, which must match); a store paired
// elsewhere re-pairs — untagging every served sentinel tag but the
// served digest's own, pruning the old epoch's sentinel rows, then
// stamping the identity; --gen must name the served generation,
// accepting the rollback as baseline. Identity-less payloads refuse
// even here — adopt blesses evidence, not silence.
//
// The re-pair deletes directly instead of through the sweeper, and
// that is the documented exception to sweeper-owns-deletes: adopt
// takes on a foreign store, not a foreign registry. The epoch cut
// is operator fiat with no verdict to exercise (no due marks, no
// TTL, no staleness applies), and it runs pre-unlock where the
// sweeper's proofs cannot exist.
func Adopt(ctx context.Context, w io.Writer, api sentinel.API, ids lineage.IdentityStore, rows adoptRows, reg adoptRegistry, armed proof.ArmedRun, identArg, genArg string) error {
	pay, servedDigest, rerr := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	if rerr != nil {
		if !sentinel.Absent(rerr) {
			return fmt.Errorf("sentinel unreadable: %v", rerr)
		}
		if identArg == "" {
			return fmt.Errorf("nothing served: run an armed `kpr store unlock` or `kpr gc` to mint a baseline first (or pass an identity to pre-pair)")
		}
		if proof.Unarmed(armed) {
			_, err := fmt.Fprintf(w, "would pair to %s with no baseline: the first mint establishes it; then `kpr store unlock` to open writes\n", identArg)
			return err
		}
		if err := ids.SetIdentity(ctx, store.Identity{ID: identArg, AdoptedAt: time.Now().UTC()}); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
		_, err := fmt.Fprintf(w, "paired to %s with no baseline: the first mint establishes it; then `kpr store unlock` to open writes\n", identArg)
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
		return repair(ctx, w, api, ids, rows, reg, armed, cur.ID, pay.ID, wantGen, servedDigest, now)
	case cur.ID != "":
		if proof.Unarmed(armed) {
			_, err = fmt.Fprintf(w, "would refresh baseline of %s to generation %s; then `kpr store unlock` to open writes\n", cur.ID, wantGen)
			return err
		}
		if err := ids.SetIdentity(ctx, store.Identity{ID: cur.ID, BaselineGen: wantGen, AdoptedAt: cur.AdoptedAt}); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
		_, err = fmt.Fprintf(w, "already paired to %s: baseline refreshed to generation %s; then `kpr store unlock` to open writes\n", cur.ID, wantGen)
		return err
	default:
		if proof.Unarmed(armed) {
			_, err = fmt.Fprintf(w, "would pair to %s at generation %s; then `kpr store unlock` to open writes\n", pay.ID, wantGen)
			return err
		}
		if err := ids.SetIdentity(ctx, store.Identity{ID: pay.ID, BaselineGen: wantGen, AdoptedAt: now}); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
		_, err = fmt.Fprintf(w, "paired to %s at generation %s; then `kpr store unlock` to open writes\n", pay.ID, wantGen)
		return err
	}
}

// fossilTag is a served sentinel tag resolving away from the served
// digest: a fossil of the abandoned epoch. Tags on the served
// digest (the floater, the served generation's own tag) are the
// proof, never fossils.
type fossilTag struct {
	repo, tag string
}

// listFossils walks the catalog's machinery namespace and resolves
// every served sentinel tag through the same read the ceremony
// trusts: same evaluation preview and arming share, so the preview
// promises what arming untags. A tag vanished between catalog and
// read is already gone (skip); anything else unreadable refuses.
func listFossils(ctx context.Context, api sentinel.API, reg adoptRegistry, servedDigest string) ([]fossilTag, error) {
	repos, err := reg.CatalogAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("sentinels unlistable: %w", err)
	}
	var fossils []fossilTag
	for _, repo := range repos {
		if !strings.HasPrefix(repo, sentinelPrefix) {
			continue
		}
		tags, err := reg.Catalog(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("sentinels unlistable: %w", err)
		}
		for _, tag := range tags {
			_, digest, rerr := sentinel.Read(ctx, api, repo, tag)
			switch {
			case rerr == nil && digest == servedDigest:
				continue
			case rerr == nil:
				fossils = append(fossils, fossilTag{repo, tag})
			case sentinel.Absent(rerr):
				continue
			default:
				return nil, fmt.Errorf("sentinel tag %s:%s unreadable: %v (remove or rename the tag, then re-run)", repo, tag, rerr)
			}
		}
	}
	return fossils, nil
}

// oldSentinelRows enumerates the abandoned epoch's tracked rows:
// every row under the machinery namespace, the same scope the
// untag walk covers. The read-only half of the prune, shared by
// preview (which counts) and arming (which deletes). Rows for the
// kept tags go too: they are the foreign epoch's tracking, not
// the new one's — the next armed gc adopt-records the served
// generation fresh.
func oldSentinelRows(ctx context.Context, rows adoptRows) ([]policy.Row, error) {
	all, err := rows.All(ctx)
	if err != nil {
		return nil, err
	}
	var doomed []policy.Row
	for _, r := range all {
		if !strings.HasPrefix(r.Repo, sentinelPrefix) {
			continue
		}
		doomed = append(doomed, r)
	}
	return doomed, nil
}

// pruneSentinelRows drops the enumerated epoch rows. It deletes
// only — enumeration is the caller's (repair lists once for both
// preview and arming), so a mid-cut re-read can never disagree
// with the announced count.
func pruneSentinelRows(ctx context.Context, rows adoptRows, doomed []policy.Row) (int, error) {
	pruned := 0
	for _, r := range doomed {
		if err := rows.Delete(ctx, r.Repo, r.Tag); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}

// repair cuts the old epoch: untag every fossil, then prune the
// old epoch's rows, then stamp the identity. Each gate refuses
// before the next writes — a failed untag leaves rows and identity
// untouched, a failed prune leaves the identity untouched. Deleted
// and gone both confirm; a held delete or any other failure
// refuses loudly for a retry to converge on.
func repair(ctx context.Context, w io.Writer, api sentinel.API, ids lineage.IdentityStore, rows adoptRows, reg adoptRegistry, armed proof.ArmedRun, oldID, newID, wantGen, servedDigest string, now time.Time) error {
	fossils, err := listFossils(ctx, api, reg, servedDigest)
	if err != nil {
		return err
	}
	doomed, err := oldSentinelRows(ctx, rows)
	if err != nil {
		return fmt.Errorf("old epoch unprunable: %w", err)
	}
	verb, summary := "untagged", "re-paired from %s to %s at generation %s: %s, %s of the old epoch; then `kpr store unlock` to open writes\n"
	if proof.Unarmed(armed) {
		verb, summary = "would untag", "would re-pair from %s to %s at generation %s: %s, %s of the old epoch; then `kpr store unlock` to open writes\n"
	}
	for _, f := range fossils {
		if _, err := fmt.Fprintf(w, "%s %s:%s\n", verb, f.repo, f.tag); err != nil {
			return err
		}
	}
	if proof.Unarmed(armed) {
		_, err = fmt.Fprintf(w, summary, oldID, newID, wantGen,
			"would untag "+words.Plural(len(fossils), "tag", "tags"),
			"would prune "+words.Plural(len(doomed), "row", "rows"))
		return err
	}
	for _, f := range fossils {
		outcome, derr := reg.DeleteManifest(ctx, f.repo, f.tag)
		switch {
		case derr != nil:
			return fmt.Errorf("old epoch un-untaggable: delete %s:%s: %w", f.repo, f.tag, derr)
		case outcome == registry.OutcomeDeleted || outcome == registry.OutcomeGone:
			continue
		default:
			return fmt.Errorf("old epoch un-untaggable: registry held the delete of %s:%s", f.repo, f.tag)
		}
	}
	pruned, err := pruneSentinelRows(ctx, rows, doomed)
	if err != nil {
		return fmt.Errorf("old epoch unprunable: %w", err)
	}
	if err := ids.SetIdentity(ctx, store.Identity{ID: newID, BaselineGen: wantGen, AdoptedAt: now}); err != nil {
		return fmt.Errorf("lineage unrecordable: %w", err)
	}
	_, err = fmt.Fprintf(w, summary, oldID, newID, wantGen,
		"untagged "+words.Plural(len(fossils), "tag", "tags"),
		"pruned "+words.Plural(pruned, "row", "rows"))
	return err
}
