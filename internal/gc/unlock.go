package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// UnlockStore records the operator's write intent: the one method
// unlock needs. store.Store satisfies it; the use case declares
// only this.
type UnlockStore interface {
	SetUnlocked(ctx context.Context, unlocked bool) error
}

// Unlock proves the shared store with a fresh generation and records
// the intent to allow registry-store writes. Read-first through the
// same verdict gc uses: silence establishes the pairing (generating
// one when unpaired — with a loud warning when the store was
// already paired, since a missing tag under a lineage means a wipe,
// not a fresh deploy); a served lineage mints under the stored
// identity; foreign, unpaired-facing-served, identity-less, stale,
// and unreadable lineages refuse with the ceremony named. The
// proof's timestamp comes from a checked clock: skew refuses
// (unlock is manual — fix the clock and retry, there is no accept
// flag to hide behind), an unreachable NTP warns and proceeds. Anything
// unproven refuses and the store stays locked.
func Unlock(ctx context.Context, w io.Writer, api sentinel.API, configPath string, st UnlockStore, rec Recorder, ids lineage.IdentityStore, rows lineage.Rows, clk clock.Source, timeServer string) error {
	fsStore, err := proof.ProveFilesystemStore(configPath)
	if err != nil {
		return err
	}
	root := fsStore.Root()
	// Same funnel as gc runs: refusals pass through untouched, so
	// the skew message and the unreachable warning below read
	// exactly as before. The ceremony consumes the gate.
	if _, cerr := (proof.Checker{Tolerance: clock.Tolerance}.Check(ctx, clk, timeServer)); cerr != nil {
		var skew *clock.SkewError
		if errors.As(cerr, &skew) {
			return fmt.Errorf("clock skew %s exceeds %s against %s: fix the clock and retry (unlock carries no accept flags)",
				skew.Offset.Round(time.Second), skew.Tolerance, timeServer)
		}
		if _, err := fmt.Fprintf(w, "Warning: time source %s unreachable (%v); proceeding with local clock\n", timeServer, cerr); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	pay, digest, rerr := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	allRows, err := rows.All(ctx)
	if err != nil {
		return fmt.Errorf("tracked state unreadable: %w", err)
	}
	ident, err := ids.GetIdentity(ctx)
	if err != nil {
		return fmt.Errorf("lineage unreadable: %w", err)
	}
	v := lineage.Judge(
		lineage.Served{Payload: pay, Digest: digest, Err: rerr},
		lineage.Local{Ident: ident, Rows: allRows},
		lineage.Ask{Now: now})
	if !v.Proceed && !v.Establish {
		return fmt.Errorf("%s — %s", v.Reason, v.Action)
	}
	useID := ident.ID
	if v.Establish {
		est := store.Identity{ID: useID, BaselineGen: ident.BaselineGen, AdoptedAt: now}
		if useID == "" {
			useID, err = sentinel.NewGen()
			if err != nil {
				return err
			}
			est.ID = useID
		} else {
			// Re-minting, not re-pairing: the acceptance the
			// operator recorded (baseline, adopted-at) survives.
			est.AdoptedAt = ident.AdoptedAt
			if _, err := fmt.Fprintf(w, "Warning: store paired to %s but nothing served — re-minting the baseline (check the mount if unintended)\n", useID); err != nil {
				return err
			}
		}
		if err := ids.SetIdentity(ctx, est); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
	}
	gen, err := sentinel.NewGen()
	if err != nil {
		return err
	}
	payload := sentinel.Payload{V: 1, Gen: gen, ID: useID, TS: now.Format(time.RFC3339), Writer: "kpr-unlock"}
	md, err := writeVerifiedGeneration(ctx, api, root, payload)
	if err != nil {
		return err
	}
	if err := rec.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: gen, Digest: md, MediaType: sentinel.ManifestMediaType, PushedAt: now, Actor: payload.Writer}); err != nil {
		return fmt.Errorf("proof held but the generation went untracked: %w", err)
	}
	if err := st.SetUnlocked(ctx, true); err != nil {
		return fmt.Errorf("proof held but the intent marker failed: %w", err)
	}
	_, err = fmt.Fprintf(w, "store unlocked: shared store proven via %s:%s generation %s\n", sentinel.Repo, sentinel.Tag, gen)
	return err
}
