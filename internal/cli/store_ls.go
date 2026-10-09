package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/helpers/human"
	"nrtn.dev/catalyst/kpr/internal/helpers/words"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registryfs"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// storeLsOpts is what `ls` shows: short age columns by default,
// the full row under --long, machinery under `ls sentinels`.
type storeLsOpts struct {
	now       time.Time
	long      bool
	sentinels bool
	json      bool
}

// isSentinelRow reports machinery rows: generations tracked under
// the sentinel repo. Exact match, like lineage and adopt — a
// prefix would hide user repos sharing the namespace
// (noroutine/kpr-web is inventory, not machinery).
func isSentinelRow(r policy.Row) bool {
	return r.Repo == sentinel.Repo
}

// runStoreLs prints tracked rows as aligned columns under headers:
// repo:tag, short age, due state. Sentinels stay out unless asked
// (`ls sentinels` shows only them); --long restores the full row.
// Sorted for stable reads.
func runStoreLs(ctx context.Context, w io.Writer, s store.Store, opts storeLsOpts) error {
	all, err := s.All(ctx)
	if err != nil {
		return err
	}
	rows := all[:0:0]
	for _, r := range all {
		if isSentinelRow(r) != opts.sentinels {
			continue
		}
		rows = append(rows, r)
	}
	// NOTE(mutants): both comparisons are equivalent — the !=
	// guard above means the operands always differ here (where <
	// and <= agree), and duplicate repo:tag rows cannot exist
	// (Record upserts by key), so the tag line never sees equals
	// either. A test distinguishing either would need rows the
	// store cannot hold.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			// NOTE(mutants): <= is equivalent — unequal repos
			// never see equals, the guard above keeps them out.
			return rows[i].Repo < rows[j].Repo
		}
		// NOTE(mutants): <= is equivalent — rows key on repo:tag,
		// so equal elements cannot occur and strictness is free.
		return rows[i].Tag < rows[j].Tag
	})
	if opts.json {
		out := make([]rowJSON, 0, len(rows))
		for _, r := range rows {
			out = append(out, rowToJSON(r))
		}
		return json.NewEncoder(w).Encode(out)
	}
	if len(rows) == 0 {
		if opts.sentinels {
			_, err := io.WriteString(w, "no sentinel rows\n")
			return err
		}
		_, err := io.WriteString(w, "no tracked rows\n")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if opts.long {
		if _, err := fmt.Fprintln(tw, "REPO:TAG\tDIGEST\tPUSHED\tACTOR\tDUE"); err != nil {
			return err
		}
		for _, r := range rows {
			state := "not due"
			if r.Due {
				state = r.Reason
			}
			if _, err := fmt.Fprintf(tw, "%s:%s\t%s\t%s\t%s\t%s\n",
				r.Repo, r.Tag, r.Digest,
				r.PushedAt.UTC().Format(time.RFC3339), r.Actor, state); err != nil {
				return err
			}
		}
		return tw.Flush()
	}
	if _, err := fmt.Fprintln(tw, "REPO:TAG\tAGE\tDUE"); err != nil {
		return err
	}
	for _, r := range rows {
		state := "not due"
		if r.Due {
			state = r.Reason
		}
		if _, err := fmt.Fprintf(tw, "%s:%s\t%s\t%s\n",
			r.Repo, r.Tag, human.ShortAge(opts.now, r.PushedAt), state); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// ghostJSON is a ghost row for piping: the stored row plus the
// evidence beside it (never merged into the row's own reason).
type ghostJSON struct {
	rowJSON
	Evidence string `json:"evidence"`
}

// runStoreGhosts prints the convergence view: tracked rows both
// witnesses agree are gone, each with its evidence. Read-only —
// rows come back untouched, so acting on one stays an operator
// decision (`store rm`). The same-store proof gates it; an empty
// fs view refuses instead of guessing. Unreadable repos and
// witness conflicts degrade to footers (names under --long, full
// lists in --json), never to ghosts. Sorted for stable reads.
// Partial answers still exit 0 — the footers say what was skipped.
func runStoreGhosts(ctx context.Context, w io.Writer, s store.Store, reg keeper.CatalogSource, fsRepos map[string]bool, same proof.SameStore, opts storeLsOpts) error {
	ghosts, conflicts, unreadable, err := keeper.ListGhosts(ctx, s, reg, fsRepos, same)
	if err != nil {
		return err
	}
	footers := []string{}
	if len(unreadable) > 0 {
		footers = append(footers, fmt.Sprintf("skipped %s (catalog unreadable)%s",
			words.Plural(len(unreadable), "repo", "repos"), ghostNames(opts.long, unreadable)))
	}
	if len(conflicts) > 0 {
		footers = append(footers, fmt.Sprintf("conflict %s (catalog lists, fs absent)%s",
			words.Plural(len(conflicts), "repo", "repos"), ghostNames(opts.long, conflicts)))
	}
	if opts.json {
		out := struct {
			Ghosts     []ghostJSON `json:"ghosts"`
			Conflicts  []string    `json:"conflicts"`
			Unreadable []string    `json:"unreadable"`
		}{Conflicts: conflicts, Unreadable: unreadable}
		for _, g := range ghosts {
			out.Ghosts = append(out.Ghosts, ghostJSON{rowJSON: rowToJSON(g.Row), Evidence: g.Evidence})
		}
		if out.Ghosts == nil {
			out.Ghosts = []ghostJSON{}
		}
		if out.Conflicts == nil {
			out.Conflicts = []string{}
		}
		if out.Unreadable == nil {
			out.Unreadable = []string{}
		}
		return json.NewEncoder(w).Encode(out)
	}
	if len(ghosts) == 0 {
		if _, err := io.WriteString(w, "no ghost rows\n"); err != nil {
			return err
		}
		for _, f := range footers {
			if _, err := io.WriteString(w, f+"\n"); err != nil {
				return err
			}
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if opts.long {
		if _, err := fmt.Fprintln(tw, "REPO:TAG\tDIGEST\tPUSHED\tACTOR\tEVIDENCE"); err != nil {
			return err
		}
		for _, g := range ghosts {
			r := g.Row
			if _, err := fmt.Fprintf(tw, "%s:%s\t%s\t%s\t%s\t%s\n",
				r.Repo, r.Tag, r.Digest,
				r.PushedAt.UTC().Format(time.RFC3339), r.Actor, g.Evidence); err != nil {
				return err
			}
		}
	} else {
		if _, err := fmt.Fprintln(tw, "REPO:TAG\tAGE\tEVIDENCE"); err != nil {
			return err
		}
		for _, g := range ghosts {
			if _, err := fmt.Fprintf(tw, "%s:%s\t%s\t%s\n",
				g.Row.Repo, g.Row.Tag, human.ShortAge(opts.now, g.Row.PushedAt), g.Evidence); err != nil {
				return err
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, f := range footers {
		if _, err := io.WriteString(w, f+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// ghostNames appends footer names under --long only: the short
// view counts, the long view accounts.
func ghostNames(long bool, repos []string) string {
	if !long || len(repos) == 0 {
		return ""
	}
	return ": " + strings.Join(repos, ", ")
}

var storeLsCmd = &cobra.Command{
	Use:   "ls [sentinels|ghosts]",
	Short: "List tracked rows (sentinels and ghosts take their own target)",
	Long: `Tracked rows as short columns: repo:tag, age, due state.
Sentinels stay out — machinery, not inventory; 'ls sentinels'
shows only them, 'ls ghosts' only rows both witnesses agree are
gone. Skipped and conflicting repos degrade to footers; partial
answers still exit 0.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 1 || (len(args) == 1 && args[0] != "sentinels" && args[0] != "ghosts") {
			return fmt.Errorf("want `ls`, `ls sentinels`, or `ls ghosts`, got %q", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		long, _ := cmd.Flags().GetBool("long")
		asJSON, _ := cmd.Flags().GetBool("json")
		if len(args) == 1 && args[0] == "ghosts" {
			// Identity first: judging rows from a foreign store is
			// the ambiguity proofs exist to refuse. The fs view
			// follows — an unprovable root refuses just as loudly.
			same, serr := proof.Prover{Sentinel: d.Reg, Store: d.Store}.Prove(cmd.Context())
			if serr != nil {
				return serr
			}
			fsStore, ferr := proof.ProveFilesystemStore(d.Cfg.RegistryConfig)
			if ferr != nil {
				return fmt.Errorf("ghosts need the fs second opinion: %w", ferr)
			}
			fsRepos, ferr := registryfs.ListRepos(fsStore)
			if ferr != nil {
				return fmt.Errorf("ghosts need the fs second opinion: %w", ferr)
			}
			return runStoreGhosts(cmd.Context(), cmd.OutOrStdout(), d.Store, d.Reg, fsRepos, same,
				storeLsOpts{now: time.Now().UTC(), long: long, json: asJSON})
		}
		return runStoreLs(cmd.Context(), cmd.OutOrStdout(), d.Store, storeLsOpts{
			now: time.Now().UTC(), long: long,
			sentinels: len(args) == 1, json: asJSON,
		})
	},
}

func init() {
	storeLsCmd.Flags().Bool("long", false, "Render the full row (digest, pushed, actor)")
	storeLsCmd.Flags().Bool("json", false, "Render rows as JSON")
	storeCmd.AddCommand(storeLsCmd)
}
