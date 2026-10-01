package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// splitRef cuts an exact repo:tag, sharing the split with
// policy.ParseExactImage (one parser for the concept —
// plan-side stays the pattern world). The wildcard refusal keeps
// store wording: ParseExactImage's message points at plan/reap,
// which would misdirect here.
func splitRef(ref string) (repo, tag string, err error) {
	if strings.ContainsAny(ref, "*?") {
		return "", "", fmt.Errorf("not an exact image %q: globs need plan add/remove, not store", ref)
	}
	return policy.ParseExactImage(ref)
}

// rowJSON is the piped shape of a tracked row.
type rowJSON struct {
	Repo      string `json:"repo"`
	Tag       string `json:"tag"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	PushedAt  string `json:"pushed_at"`
	Actor     string `json:"actor"`
	Due       bool   `json:"due"`
	Reason    string `json:"reason,omitempty"`
}

func rowToJSON(r policy.Row) rowJSON {
	return rowJSON{Repo: r.Repo, Tag: r.Tag, Digest: r.Digest,
		MediaType: r.MediaType, PushedAt: r.PushedAt.UTC().Format(time.RFC3339),
		Actor: r.Actor, Due: r.Due, Reason: r.Reason}
}

// runStoreLs prints every tracked row: the lay of the field plan
// cannot show (plan lists due marks only). Sorted for stable reads.
func runStoreLs(ctx context.Context, w io.Writer, s store.Store, asJSON bool) error {
	rows, err := s.All(ctx)
	if err != nil {
		return err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		return rows[i].Tag < rows[j].Tag
	})
	if asJSON {
		out := make([]rowJSON, 0, len(rows))
		for _, r := range rows {
			out = append(out, rowToJSON(r))
		}
		return json.NewEncoder(w).Encode(out)
	}
	if len(rows) == 0 {
		_, err := io.WriteString(w, "no tracked rows\n")
		return err
	}
	for _, r := range rows {
		state := "not due"
		if r.Due {
			state = "due: " + r.Reason
		}
		if _, err := fmt.Fprintf(w, "%s:%s %s pushed=%s actor=%s %s\n",
			r.Repo, r.Tag, r.Digest,
			r.PushedAt.UTC().Format(time.RFC3339), r.Actor, state); err != nil {
			return err
		}
	}
	return nil
}

// runStoreInspect prints the full row for one exact repo:tag. An
// untracked spelling refuses naming it — never a near miss.
func runStoreInspect(ctx context.Context, w io.Writer, s store.Store, ref string, asJSON bool) error {
	repo, tag, err := splitRef(ref)
	if err != nil {
		return err
	}
	rows, err := s.All(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Repo != repo || r.Tag != tag {
			continue
		}
		if asJSON {
			return json.NewEncoder(w).Encode(rowToJSON(r))
		}
		_, err := fmt.Fprintf(w, "repo: %s\ntag: %s\ndigest: %s\nmedia_type: %s\npushed_at: %s\nactor: %s\ndue: %v\nreason: %s\n",
			r.Repo, r.Tag, r.Digest, r.MediaType,
			r.PushedAt.UTC().Format(time.RFC3339), r.Actor, r.Due, r.Reason)
		return err
	}
	return fmt.Errorf("no tracked row %s:%s", repo, tag)
}

// runStoreRm drops tracked rows outright (Delete, not UnmarkDue:
// the row is gone, not just its mark). All-or-nothing: every ref
// resolves before the first delete, so a typo cannot half-clear
// the store. The registry tag survives, untracked until a re-push
// or backfill re-tracks it — the warning says so loudly.
func runStoreRm(ctx context.Context, w io.Writer, s store.Store, refs []string) error {
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
	held := make(map[key]bool, len(rows))
	for _, r := range rows {
		held[key{r.Repo, r.Tag}] = true
	}
	for _, k := range keys {
		if !held[k] {
			return fmt.Errorf("no tracked row %s:%s: nothing removed", k.repo, k.tag)
		}
	}
	for _, k := range keys {
		if err := s.Delete(ctx, k.repo, k.tag); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "removed %s:%s (registry tag left untracked — re-push or backfill re-tracks)\n",
			k.repo, k.tag); err != nil {
			return err
		}
	}
	return nil
}

var storeCmd = &cobra.Command{
	Use:   "store",
	Short: "Inspect and prune tracked store rows",
	Long: `Lay of the field for tracked state: 'store ls' lists every
row (plan shows due marks only), 'store inspect' shows one full
row, 'store rm' drops rows outright. rm removes tracking only —
the registry tag survives, untracked until a re-push or backfill
re-tracks it. 'store lock' / 'store unlock' gate registry-store
writes behind a fresh proof; 'store adopt' pairs the lineage.
Backfill lands here as its own milestone.`,
}

var storeLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List every tracked row",
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		asJSON, _ := cmd.Flags().GetBool("json")
		return runStoreLs(cmd.Context(), cmd.OutOrStdout(), d.store, asJSON)
	},
}

var storeInspectCmd = &cobra.Command{
	Use:   "inspect <repo:tag>",
	Short: "Show one full tracked row",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		asJSON, _ := cmd.Flags().GetBool("json")
		return runStoreInspect(cmd.Context(), cmd.OutOrStdout(), d.store, args[0], asJSON)
	},
}

var storeRmCmd = &cobra.Command{
	Use:   "rm <repo:tag>...",
	Short: "Drop tracked rows (tag stays, untracked)",
	Long: `Delete tracked rows outright: exact repo:tag spellings only
(no globs — this is destructive), all-or-nothing (one unknown
ref refuses before anything is deleted). The registry tag is
untouched and goes untracked until a re-push or backfill
re-tracks it. A direct store edit: no dry-run.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		return runStoreRm(cmd.Context(), cmd.OutOrStdout(), d.store, args)
	},
}

func init() {
	storeLsCmd.Flags().Bool("json", false, "Render rows as JSON")
	storeInspectCmd.Flags().Bool("json", false, "Render the row as JSON")
	storeCmd.AddCommand(storeLsCmd, storeInspectCmd, storeRmCmd)
	RootCmd.AddCommand(storeCmd)
}
