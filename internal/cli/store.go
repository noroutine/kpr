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
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/helpers/words"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registryfs"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
	"nrtn.dev/catalyst/kpr/internal/trust"
)

// OpenStore opens state the same way every command does. The logic lives in
// cli/deps so subcommand packages wire without importing the command tree;
// this forwarder keeps the e2e suite (outside internal/cli, which cannot see
// deps) on that same path.
func OpenStore(cfg *config.Config) (store.StoreCloser, error) {
	backend, dir, err := deps.ResolveStoreBackend()
	if err != nil {
		return nil, err
	}
	return deps.OpenStore(cfg, backend, dir)
}

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

// shortAge renders a push time as a compact duration ("2h5m ago").
// A future stamp clamps to zero — the row is odd, the rendering
// must not be.
func shortAge(now, pushed time.Time) string {
	d := now.Sub(pushed)
	// NOTE(mutants): <= is equivalent — clamping an exactly-zero
	// age to zero is identity.
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String() + " ago"
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
			r.Repo, r.Tag, shortAge(opts.now, r.PushedAt), state); err != nil {
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
				g.Row.Repo, g.Row.Tag, shortAge(opts.now, g.Row.PushedAt), g.Evidence); err != nil {
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

// identityState voices the lineage pairing in one line: the id the
// verdicts judge the served generation against, with the accepted
// baseline when one was recorded. Unpaired names itself; a read
// failure is unknown, never a guess.
func identityState(ctx context.Context, s store.Store) string {
	id, err := s.GetIdentity(ctx)
	if err != nil {
		return "unknown"
	}
	if id.ID == "" {
		return "unpaired"
	}
	if id.BaselineGen != "" {
		return id.ID + " (baseline " + id.BaselineGen + ")"
	}
	return id.ID
}

// activityTail is how many ring records the text view shows: the
// newest slice, with the total beside it. The full ring (at most
// ActivityCap small records) goes to --json.
const activityTail = 10

type activityJSON struct {
	Repo    string `json:"repo"`
	Tag     string `json:"tag"`
	Reason  string `json:"reason"`
	Outcome string `json:"outcome"`
	At      string `json:"at"`
	Actor   string `json:"actor,omitempty"`
	Trigger string `json:"trigger,omitempty"`
}

// runStoreStatus prints the store card the banner gave up: backend,
// intent, live proof, pairing, and the activity tail. A read failure
// on the ring degrades to unknown — the card above it already
// answered.
func runStoreStatus(ctx context.Context, w io.Writer, s store.Store, api sentinel.API, storeLine string, asJSON bool) error {
	status := "unproven"
	if rows, rerr := s.All(ctx); rerr == nil {
		if id, ierr := s.GetIdentity(ctx); ierr == nil {
			status = trust.Word(ctx, api, id, rows)
		}
	}
	lock := lockState(ctx, s)
	proof := proofState(ctx, api)
	ident := identityState(ctx, s)
	acts, actErr := s.Activity(ctx)
	if asJSON {
		out := struct {
			Status   string         `json:"status"`
			Store    string         `json:"store"`
			Lock     string         `json:"lock"`
			Proof    string         `json:"proof"`
			Identity string         `json:"identity"`
			Activity []activityJSON `json:"activity"`
		}{Status: status, Store: storeLine, Lock: lock, Proof: proof, Identity: ident}
		// NOTE(mutants): == is equivalent — every backend returns
		// nil rows with a read error (redis/file) or never errors
		// (mem), so ranging on the error path appends nothing either
		// way. A test feeding rows+error together would enshrine a
		// combination no backend produces.
		if actErr == nil {
			for _, a := range acts {
				out.Activity = append(out.Activity, activityJSON{Repo: a.Repo, Tag: a.Tag,
					Reason: a.Reason, Outcome: a.Outcome, At: a.At.UTC().Format(time.RFC3339),
					Actor: a.Actor, Trigger: a.Trigger})
			}
		}
		return json.NewEncoder(w).Encode(out)
	}
	if _, err := fmt.Fprintf(w, "status: %s\nstore: %s\nstore-lock: %s\nproof: %s\nidentity: %s\n",
		status, storeLine, lock, proof, ident); err != nil {
		return err
	}
	if actErr != nil {
		_, err := io.WriteString(w, "activity: unknown\n")
		return err
	}
	if len(acts) == 0 {
		_, err := io.WriteString(w, "activity: none recorded\n")
		return err
	}
	n := min(len(acts), activityTail)
	if _, err := fmt.Fprintf(w, "activity (last %d of %d):\n", n, len(acts)); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, a := range acts[:n] {
		if _, err := fmt.Fprintln(w, renderOutcome(now, a)); err != nil {
			return err
		}
	}
	return nil
}

// renderOutcome prints one ring entry: rows as repo:tag —
// outcome, row-less control events (fence flips) as the outcome
// alone, each with a human age (precise stamps stay in --json).
// A stray ": — " prefix reads as a malformed row.
func renderOutcome(now time.Time, a store.Outcome) string {
	age := "unknown age"
	if !a.At.IsZero() {
		age = shortAge(now, a.At)
	}
	if a.Repo == "" && a.Tag == "" {
		return fmt.Sprintf("  %s (%s), %s", a.Outcome, a.Reason, age)
	}
	return fmt.Sprintf("  %s:%s — %s (%s), %s", a.Repo, a.Tag, a.Outcome, a.Reason, age)
}

var storeCmd = &cobra.Command{
	Use:   "store",
	Short: "Inspect and prune tracked store rows",
	Long: `Lay of the field for tracked state: 'store ls' lists
tracked rows short (sentinels take 'ls sentinels', ghosts agreed
gone by catalog and fs take 'ls ghosts', --long the
full row), 'store inspect' shows one full row, 'store rm' drops
rows outright. rm removes tracking only —
the registry tag survives, untracked until a re-push or backfill
re-tracks it. 'store lock' / 'store unlock' gate registry-store
writes behind a fresh proof; 'store adopt' pairs the lineage;
'store backfill' adopts pre-kpr tags into tracked rows.`,
}

var storeLsCmd = &cobra.Command{
	Use:   "ls [sentinels|ghosts]",
	Short: "List tracked rows (sentinels and ghosts take their own target)",
	Long: `Tracked rows as short aligned columns: repo:tag, age, due
state. Sentinel generations stay out — machinery, not inventory;
` + "`ls sentinels`" + ` shows only them (same columns, wider
names). ` + "`ls ghosts`" + ` shows only rows both witnesses agree
are gone (catalog 404, fs absent), each with its evidence —
read-only, acting on one stays plain ` + "`store rm`" + `'s job
(the tag is already gone upstream, so ` + "`--untag`" + ` has
nothing to delete). Skipped and conflicting repos degrade to
footers, never to ghosts; partial answers still exit 0. --long
restores the full row (digest, pushed, actor).`,
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

var storeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show trust, backend, lock, proof, identity, and activity",
	Long: `The store card: the trust word on top (the same verdict
analyze parentheses — paired, or why mistrusted), the wired
backend, intent marker, live proof generation with its age, the
lineage pairing the verdicts judge against, and the tail of the
activity ring (the sweeper's per-row outcomes — what the counters
count). --json renders it for piping.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		return runStoreStatus(cmd.Context(), cmd.OutOrStdout(), d.Store, d.Reg, describeStore(d.Store, d.Cfg), asJSON)
	},
}

var storeInspectCmd = &cobra.Command{
	Use:   "inspect <repo:tag>",
	Short: "Show one full tracked row",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		asJSON, _ := cmd.Flags().GetBool("json")
		return runStoreInspect(cmd.Context(), cmd.OutOrStdout(), d.Store, args[0], asJSON)
	},
}

var storeRmCmd = &cobra.Command{
	Use:   "rm <repo:tag>...",
	Short: "Drop tracked rows (tag stays, untracked)",
	Long: `Delete tracked rows outright: exact repo:tag spellings only
(no globs — this is destructive), all-or-nothing (one unknown
ref refuses before anything is deleted). Bare rm removes tracking
only: the registry tag goes untracked until a re-push or backfill
re-tracks it. --untag deletes the manifest by digest first (the
sweeper's order and path) and drops the row only on confirm; a
held or failed delete keeps its row loudly. Either way blob bytes
still need 'kpr gc'. A direct store edit: no dry-run.`,
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
		// so it mints first — a foreign or stale store refuses here,
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
	storeLsCmd.Flags().Bool("long", false, "Render the full row (digest, pushed, actor)")
	storeLsCmd.Flags().Bool("json", false, "Render rows as JSON")
	storeInspectCmd.Flags().Bool("json", false, "Render the row as JSON")
	storeStatusCmd.Flags().Bool("json", false, "Render the card as JSON")
	storeRmCmd.Flags().Bool("untag", false, "Delete the registry manifest too (by digest), dropping the row only on confirm")
	storeCmd.AddCommand(storeLsCmd, storeInspectCmd, storeRmCmd, storeStatusCmd)
	RootCmd.AddCommand(storeCmd)
}
