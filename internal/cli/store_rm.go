package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// runStoreRm drops tracked rows outright (Delete, not UnmarkDue:
// the row is gone, not just its mark). All-or-nothing: every ref
// resolves before the first delete, so a typo cannot half-clear
// the store. Bare rm removes tracking only — the registry tag
// survives, untracked until a re-push or backfill re-tracks it.
// With untag, deletion delegates to the sweep use case
// (`Sweeper.Untag` — the only deleter left): manifest by digest
// first, row drops only on confirm, held/failed keeps its row
// loudly. Blob bytes still need `gc` after — untag unlinks, never
// collects.
func runStoreRm(ctx context.Context, w io.Writer, s store.Store, refs []string, untag bool, sw *sweep.Sweeper, same proof.SameStore, unlocked proof.UnlockedStore) error {
	type key struct{ repo, tag string }
	var keys []key
	for _, ref := range refs {
		repo, tag, err := splitRef(ref)
		if err != nil {
			return err
		}
		keys = append(keys, key{repo, tag})
	}
	rows, err := s.All(ctx)
	if err != nil {
		return err
	}
	byKey := make(map[key]policy.Row, len(rows))
	for _, r := range rows {
		byKey[key{r.Repo, r.Tag}] = r
	}
	for _, k := range keys {
		if _, ok := byKey[k]; !ok {
			return fmt.Errorf("no tracked row %s:%s: nothing removed", k.repo, k.tag)
		}
	}
	if !untag {
		var targets []policy.Row
		for _, k := range keys {
			targets = append(targets, byKey[k])
		}
		done, uerr := sw.Untrack(ctx, targets, unlocked)
		for _, r := range done {
			if _, err := fmt.Fprintf(w, "removed %s:%s (registry tag left untracked — re-push or backfill re-tracks)\n",
				r.Repo, r.Tag); err != nil {
				return err
			}
		}
		return uerr
	}
	var targets []policy.Row
	for _, k := range keys {
		targets = append(targets, byKey[k])
	}
	done, uerr := sw.Untag(ctx, targets, same, unlocked)
	for _, r := range done {
		if _, err := fmt.Fprintf(w, "untagged %s:%s (row dropped; blobs need `gc`)\n",
			r.Repo, r.Tag); err != nil {
			return err
		}
	}
	return uerr
}

var storeRmCmd = &cobra.Command{
	Use:   "rm <repo:tag>...",
	Short: "Drop tracked rows (tag stays, untracked)",
	Long: `Delete tracked rows outright: exact repo:tag spellings only,
all-or-nothing. Bare rm removes tracking only — the tag survives
untracked. --untag deletes the manifest first and drops the row
only on confirm.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		untag, _ := cmd.Flags().GetBool("untag")
		sw := &sweep.Sweeper{Store: d.Store, Registry: d.Reg}
		// The caller proves intent first: both rm paths drop rows, so
		// both need the marker — a locked store refuses here, naming
		// the ceremony, before anything is forgotten or deleted.
		unlocked, err := proof.ProveUnlockedStore(cmd.Context(), d.Store)
		if err != nil {
			return err
		}
		// Identity rides the second token, untag only: bare rm forgets
		// tracking (proof-free), but --untag deletes from the registry,
		// so it proves first — a foreign or stale store refuses here,
		// before the first manifest.
		var same proof.SameStore
		if untag {
			var err error
			same, err = proof.Prover{Sentinel: d.Reg, Store: d.Store}.Prove(cmd.Context())
			if err != nil {
				return err
			}
		}
		return runStoreRm(cmd.Context(), cmd.OutOrStdout(), d.Store, args, untag, sw, same, unlocked)
	},
}

func init() {
	storeRmCmd.Flags().Bool("untag", false, "Delete the registry manifest too (by digest), dropping the row only on confirm")
	storeCmd.AddCommand(storeRmCmd)
}
