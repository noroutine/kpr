package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/registryfs"
)

var registryAnalyzeJSON bool

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Inspect the distribution registry itself",
}

var registryAnalyzeCmd = &cobra.Command{
	Use:   "analyze",
	Short: "Report registry store magnitude, fs vs API",
	Long: `Report magnitude as five live lines, grouped by sense: the
fast catalog view first (repos, tags — a rough size up front),
then the fs-vs-catalog shape, manifests, blobs, and bytes. The
fs line carries the running fs-minus-catalog delta — negative
while the walk counts up, converging on the skew. The API has
no endpoints for revisions, blobs, uploads, or layer links, so
those stay fs-side. Read-only and verdict-free: the live block
is the display, nothing reprints it. Needs the filestore proof
(KPR_REGISTRY_CONFIG names a config with a filesystem storage
root). Point-in-time on a live registry.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// No openDeps: analyze never touches the state store — a
		// down redis must not refuse a read-only walk. The
		// registry client dials nothing at build (same comment
		// as deps), so constructing it directly is harmless.
		cfg := config.NewBuilder().FromEnv().Build()
		reg := registry.NewClient(cfg.RegistryURL)
		reg.SetBasicAuth(cfg.RegistryUser, cfg.RegistryPassword)
		return runRegistryAnalyze(cmd.Context(), cmd.OutOrStdout(), cfg.RegistryConfig, reg, registryAnalyzeJSON)
	},
}

// analyzeJSON is the piped shape of a magnitude report: the fs
// walk plus the two numbers the API walk sees.
type analyzeJSON struct {
	Repos          int   `json:"repos"`
	Tags           int   `json:"tags"`
	Revisions      int   `json:"revisions"`
	Blobs          int   `json:"blobs"`
	BlobBytes      int64 `json:"blob_bytes"`
	Uploads        int   `json:"uploads"`
	LayerLinks     int   `json:"layer_links"`
	Sentinels      int   `json:"sentinels"`
	StoreRepos     int   `json:"store_repos"`
	StoreTags      int   `json:"store_tags"`
	StoreSentinels int   `json:"store_sentinels"`
	StoreOK        bool  `json:"store_ok"`
	APIRepos       int   `json:"api_repos"`
	APITags        int   `json:"api_tags"`
	APISentinels   int   `json:"api_sentinels"`
}

// humanBytes renders bytes in the largest binary unit that keeps
// the value at one or more whole units, two decimals: bytes stay
// bytes, gibibytes stay gibibytes. Exact bytes stay in --json,
// humans get a sense of scale at any magnitude.
func humanBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	v := float64(b)
	unit := "B"
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		v /= 1024
		unit = u
		if v < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.2f %s", v, unit)
}

// analyzeRow labels one magnitude line: names pad to one width so
// values start in one column down the whole block.
func analyzeRow(name, body string) string {
	return fmt.Sprintf("%-7s: %s", name, body)
}

// plural renders a counted noun: 1 repo, 2 repos. Participles
// (tracked, recorded, untagged) and substance labels (GiB blobs)
// stay invariant — only true nouns pluralize.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// catalogLine renders what the API names, sentinels apart: the
// machinery footprint reads on both views, and the comparison
// only holds when both sides split it the same way.
func catalogLine(api backfill.CatalogReport) string {
	return analyzeRow("catalog", fmt.Sprintf("%s, %s, %s",
		plural(api.Repos, "repo", "repos"),
		plural(api.Tags, "tag", "tags"),
		plural(api.Sentinels, "sentinel", "sentinels")))
}

// analyzeLines renders the five-line block, grouped by sense:
// catalog shape, fs-vs-catalog shape, manifests, blobs, bytes.
// Sentinel tags split out on both views: registry consistency
// first, so the machinery footprint and its mismatch read
// directly. The fs deltas are fs-minus-catalog — negative while
// the walk counts up, converging on the skew. Untagged is
// revisions minus fs tags, clamped at zero: a live registry can
// push tags mid-walk, and negative untagged is nonsense, not
// information. Layer links ride the blobs line:
// links-per-blob reads straight off it. The gap numbers are
// information, not verdicts: a live registry shifts under both
// walks, and empty-but-listed repos legitimately differ.
// storeView is the tracked-state half of the comparison: a static
// snapshot of `store ls`, read once before the slow walks.
type storeView struct {
	ok                     bool
	repos, tags, sentinels int
}

// summarizeRows groups tracked rows the store line's way:
// distinct repos over everything, tags over everything, sentinel
// rows split out as a memo. Tags stay inclusive so the Δ tags
// compares against the catalog's inclusive count — an exclusive
// count reads the sentinel memo as adoption debt.
func summarizeRows(rows []policy.Row) (repos, tags, sentinels int) {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Repo] = true
		tags++
		if strings.HasPrefix(r.Repo, backfill.SentinelPrefix) {
			sentinels++
		}
	}
	return len(seen), tags, sentinels
}

// storeLine renders the tracked state against the catalog, deltas
// store-minus-API: what adoption and sweeping still owe the
// registry. Unavailable reads honest when the backend is down —
// a read-only magnitude never refuses over it.
func storeLine(view storeView, api backfill.CatalogReport) string {
	if !view.ok {
		return analyzeRow("store", "unavailable")
	}
	return analyzeRow("store", fmt.Sprintf("%s, %s, %s, Δ repos: %+d, Δ tags: %+d, Δ sentinels: %+d",
		plural(view.repos, "repo", "repos"),
		plural(view.tags, "tag", "tags"),
		plural(view.sentinels, "sentinel", "sentinels"),
		view.repos-api.Repos, view.tags-api.Tags, view.sentinels-api.Sentinels))
}

// readStoreView snapshots tracked rows for the store line.
// Best-effort by design: analyze never touches the state store
// for its registry half, so a down backend degrades one line
// instead of refusing the walk.
func readStoreView(ctx context.Context) storeView {
	d, err := openDeps()
	if err != nil {
		return storeView{}
	}
	defer d.close()
	rows, err := d.store.All(ctx)
	if err != nil {
		return storeView{}
	}
	repos, tags, sentinels := summarizeRows(rows)
	return storeView{ok: true, repos: repos, tags: tags, sentinels: sentinels}
}

func analyzeLines(api backfill.CatalogReport, fs registryfs.Report, store storeView, delta bool) []string {
	fsBody := fmt.Sprintf("%s, %s, %s",
		plural(fs.Repos, "repo", "repos"),
		plural(fs.Tags, "tag", "tags"),
		plural(fs.Sentinels, "sentinel", "sentinels"))
	if delta {
		fsBody += fmt.Sprintf(", Δ repos: %+d, Δ tags: %+d, Δ sentinels: %+d",
			fs.Repos-api.Repos, fs.Tags-api.Tags, fs.Sentinels-api.Sentinels)
	}
	untagged := fs.Revisions - fs.Tags
	if untagged < 0 {
		untagged = 0
	}
	return []string{
		catalogLine(api),
		storeLine(store, api),
		analyzeRow("fs", fsBody),
		analyzeRow("revs", fmt.Sprintf("%s, %d untagged",
			plural(fs.Revisions, "revision", "revisions"), untagged)),
		analyzeRow("blobs", fmt.Sprintf("%s, %s, %s",
			plural(fs.Blobs, "blob", "blobs"),
			plural(fs.LayerLinks, "layer link", "layer links"),
			plural(fs.Uploads, "upload", "uploads"))),
		analyzeRow("size", fmt.Sprintf("%s blobs", humanBytes(fs.BlobBytes))),
	}
}

// pendingBlock holds the block while the catalog view runs first:
// the fs walk those lines report on hasn't started yet. The
// catalog line is already live and the store snapshot predates
// the scan — sentinels on both.
func pendingBlock(api backfill.CatalogReport, store storeView) []string {
	lines := []string{catalogLine(api), storeLine(store, api)}
	for _, name := range []string{"fs", "revs", "blobs", "size"} {
		lines = append(lines, analyzeRow(name, "walk pending"))
	}
	return lines
}

// runRegistryAnalyze proves the filestore, snapshots the tracked
// store (static, cheap — a different view on `store ls`), runs
// the fast catalog view (rough size up front), then the slow fs
// walk beneath both, all repainting one six-line block — the
// block is the display, nothing reprints it. Off-terminal the
// catalog and store lines flush right after their walks (cheap
// and fast) and the rest follows the fs walk; the settled lines
// never repeat either. The fs walker takes the token, never a
// bare path. The catalog pass reuses backfill's enumeration
// port. A refused catalog walk fails the command: a comparison
// against an unknown view misleads. A dead store backend only
// degrades the store line — the registry half still reports.
func runRegistryAnalyze(ctx context.Context, w io.Writer, configPath string, reg backfill.Registry, asJSON bool) error {
	fsStore, err := proof.ProveFilesystemStore(configPath)
	if err != nil {
		return err
	}
	store := readStoreView(ctx)
	// JSON is for scripts: the live block goes nowhere, only the
	// encoded report reaches w.
	liveW := w
	if asJSON {
		liveW = io.Discard
	}
	live := newLiveLines(liveW)
	api, err := backfill.ScanCatalog(ctx, io.Discard, reg, func(running backfill.CatalogReport) {
		live.tickBlock(pendingBlock(running, store))
	})
	if err != nil {
		return err
	}
	head := 0
	if !asJSON && !live.terminal() {
		for _, line := range analyzeLines(api, registryfs.Report{}, store, false)[:2] {
			_, _ = fmt.Fprintln(w, line)
		}
		head = 2
	}
	rep, err := registryfs.Analyze(fsStore, func(running registryfs.Report) {
		live.tickBlock(analyzeLines(api, running, store, true))
	})
	if err != nil {
		return err
	}
	live.doneBlock(analyzeLines(api, rep, store, true), head)
	if asJSON {
		return json.NewEncoder(w).Encode(analyzeJSON{
			Repos: rep.Repos, Tags: rep.Tags, Revisions: rep.Revisions,
			Blobs: rep.Blobs, BlobBytes: rep.BlobBytes,
			Uploads: rep.Uploads, LayerLinks: rep.LayerLinks,
			Sentinels:  rep.Sentinels,
			StoreRepos: store.repos, StoreTags: store.tags,
			StoreSentinels: store.sentinels, StoreOK: store.ok,
			APIRepos: api.Repos, APITags: api.Tags, APISentinels: api.Sentinels,
		})
	}
	return nil
}

func init() {
	registryAnalyzeCmd.Flags().BoolVar(&registryAnalyzeJSON, "json", false, "Emit the report as JSON for scripts")
	registryCmd.AddCommand(registryAnalyzeCmd)
	RootCmd.AddCommand(registryCmd)
}
