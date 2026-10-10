package storeops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// unlockStore records the operator's write intent: the one method
// unlock needs. store.Store satisfies it; the use case declares
// only this.
type unlockStore interface {
	SetUnlocked(ctx context.Context, unlocked bool) error
}

// UnlockDeps carries unlock's world: the registry API, the store in
// its recorder, identity, and rows roles, the whole store behind
// them for the intent marker, and the clock the mint checks. A nil
// clock derives from config.Current(); the filesystem root and the
// time server resolve there too — config rides no field, and tests
// stage it the same way production reads it. Unlock takes no flags
// and no acceptances, so there is no Options beside it.
type UnlockDeps struct {
	Rec   Recorder
	Ids   lineage.IdentityStore
	Rows  lineage.Rows
	API   sentinel.API
	Clock clock.Source
	// Store is the whole store behind the split roles above: the
	// intent marker unlock records.
	Store unlockStore
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
func Unlock(ctx context.Context, w io.Writer, d UnlockDeps) error {
	cfg := config.Current()
	fsStore, err := proof.ProveFilesystemStore(cfg.RegistryConfig)
	if err != nil {
		return err
	}
	root := fsStore.Root()
	// Same funnel as gc runs: refusals pass through untouched, so
	// the skew message and the unreachable warning below read
	// exactly as before. The ceremony consumes the gate.
	clk := d.Clock
	if clk == nil {
		clk = clock.NewSource(cfg.TimeMethod)
	}
	if _, cerr := (proof.Checker{Tolerance: clock.Tolerance}.Check(ctx, clk, cfg.TimeServer)); cerr != nil {
		var skew *clock.SkewError
		if errors.As(cerr, &skew) {
			return fmt.Errorf("clock skew %s exceeds %s against %s: fix the clock and retry (unlock carries no accept flags)",
				skew.Offset.Round(time.Second), skew.Tolerance, cfg.TimeServer)
		}
		if _, err := fmt.Fprintf(w, "Warning: time source %s unreachable (%v); proceeding with local clock\n", cfg.TimeServer, cerr); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
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
		// Armed lit loud: unlock heals the pairing write, so the
		// zero-value preview would silently drop the adopt row.
		lineage.Ask{Armed: true, Now: now})
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
		if err := d.Ids.SetIdentity(ctx, est); err != nil {
			return fmt.Errorf("lineage unrecordable: %w", err)
		}
	}
	gen, err := sentinel.NewGen()
	if err != nil {
		return err
	}
	payload := sentinel.Payload{V: 1, Gen: gen, ID: useID, TS: now.Format(time.RFC3339), Writer: "kpr-unlock"}
	md, err := sentinel.WriteVerified(ctx, d.API, root, payload)
	if err != nil {
		return err
	}
	if err := d.Rec.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: gen, Digest: md, MediaType: sentinel.ManifestMediaType, PushedAt: now, Actor: payload.Writer}); err != nil {
		return fmt.Errorf("proof held but the generation went untracked: %w", err)
	}
	if err := d.Store.SetUnlocked(ctx, true); err != nil {
		return fmt.Errorf("proof held but the intent marker failed: %w", err)
	}
	_, err = fmt.Fprintf(w, "store unlocked: shared store proven via %s:%s generation %s\n", sentinel.Repo, sentinel.Tag, gen)
	return err
}
