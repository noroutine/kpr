package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/config"
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
	Repos      int   `json:"repos"`
	Tags       int   `json:"tags"`
	Revisions  int   `json:"revisions"`
	Blobs      int   `json:"blobs"`
	BlobBytes  int64 `json:"blob_bytes"`
	Uploads    int   `json:"uploads"`
	LayerLinks int   `json:"layer_links"`
	MetaBytes  int64 `json:"meta_bytes"`
	APIRepos   int   `json:"api_repos"`
	APITags    int   `json:"api_tags"`
}

// gib renders bytes as fractional GiB with two decimals: exact
// bytes stay in --json, humans get a sense of scale.
func gib(b int64) string {
	return fmt.Sprintf("%.2f GiB", float64(b)/1024/1024/1024)
}

// mib renders bytes as fractional MiB with two decimals: the
// metadata scale, where GiB would read 0.00.
func mib(b int64) string {
	return fmt.Sprintf("%.2f MiB", float64(b)/1024/1024)
}

// analyzeRow labels one magnitude line: names pad to one width so
// values start in one column down the whole block.
func analyzeRow(name, body string) string {
	return fmt.Sprintf("%-7s: %s", name, body)
}

// analyzeLines renders the five-line block, grouped by sense:
// catalog shape, fs-vs-catalog shape, manifests, blobs, bytes.
// The fs deltas are fs-minus-catalog — negative while the walk
// counts up, converging on the skew. Untagged is revisions minus
// fs tags, clamped at zero: a live registry can push tags
// mid-walk, and negative untagged is nonsense, not information.
// Layer links ride the blobs line: links-per-blob reads straight
// off it. The gap numbers are information, not verdicts: a live
// registry shifts under both walks, and empty-but-listed repos
// legitimately differ.
func analyzeLines(api backfill.CatalogReport, fs registryfs.Report, delta bool) []string {
	fsBody := fmt.Sprintf("%d repos, %d tags", fs.Repos, fs.Tags)
	if delta {
		fsBody += fmt.Sprintf(", Δ repos: %+d, Δ tags: %+d", fs.Repos-api.Repos, fs.Tags-api.Tags)
	}
	untagged := fs.Revisions - fs.Tags
	if untagged < 0 {
		untagged = 0
	}
	return []string{
		analyzeRow("catalog", fmt.Sprintf("%d repos, %d tags", api.Repos, api.Tags)),
		analyzeRow("fs", fsBody),
		analyzeRow("revs", fmt.Sprintf("%d revisions, %d untagged", fs.Revisions, untagged)),
		analyzeRow("blobs", fmt.Sprintf("%d blobs, %d layer links, %d uploads", fs.Blobs, fs.LayerLinks, fs.Uploads)),
		analyzeRow("size", fmt.Sprintf("%s blobs, %s metadata", gib(fs.BlobBytes), mib(fs.MetaBytes))),
	}
}

// pendingBlock holds the block while the catalog view runs first:
// the walk those lines report on hasn't started yet.
func pendingBlock(api backfill.CatalogReport) []string {
	lines := []string{analyzeRow("catalog", fmt.Sprintf("%d repos, %d tags", api.Repos, api.Tags))}
	for _, name := range []string{"fs", "revs", "blobs", "size"} {
		lines = append(lines, analyzeRow(name, "walk pending"))
	}
	return lines
}

// runRegistryAnalyze proves the filestore, runs the fast catalog
// view first (rough size up front), then the slow fs walk beneath
// it, both repainting one five-line block — the block is the
// display, nothing reprints it. Off-terminal the catalog line
// flushes right after its walk (cheap and fast) and the rest
// follows the fs walk; the settled lines never repeat either.
// The fs walker takes the token, never a bare path. The catalog
// pass reuses backfill's enumeration port. A refused catalog walk
// fails the command: a comparison against an unknown view
// misleads.
func runRegistryAnalyze(ctx context.Context, w io.Writer, configPath string, reg backfill.Registry, asJSON bool) error {
	fsStore, err := proof.ProveFilesystemStore(configPath)
	if err != nil {
		return err
	}
	// JSON is for scripts: the live block goes nowhere, only the
	// encoded report reaches w.
	liveW := w
	if asJSON {
		liveW = io.Discard
	}
	live := newLiveLines(liveW)
	api, err := backfill.ScanCatalog(ctx, io.Discard, reg, func(running backfill.CatalogReport) {
		live.tickBlock(pendingBlock(running))
	})
	if err != nil {
		return err
	}
	head := 0
	if !asJSON && !live.terminal() {
		_, _ = fmt.Fprintln(w, analyzeLines(api, registryfs.Report{}, false)[0])
		head = 1
	}
	rep, err := registryfs.Analyze(fsStore, func(running registryfs.Report) {
		live.tickBlock(analyzeLines(api, running, true))
	})
	if err != nil {
		return err
	}
	live.doneBlock(analyzeLines(api, rep, true), head)
	if asJSON {
		return json.NewEncoder(w).Encode(analyzeJSON{
			Repos: rep.Repos, Tags: rep.Tags, Revisions: rep.Revisions,
			Blobs: rep.Blobs, BlobBytes: rep.BlobBytes,
			Uploads: rep.Uploads, LayerLinks: rep.LayerLinks,
			MetaBytes: rep.MetaBytes,
			APIRepos:  api.Repos, APITags: api.Tags,
		})
	}
	return nil
}

func init() {
	registryAnalyzeCmd.Flags().BoolVar(&registryAnalyzeJSON, "json", false, "Emit the report as JSON for scripts")
	registryCmd.AddCommand(registryAnalyzeCmd)
	RootCmd.AddCommand(registryCmd)
}
