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
	Long: `Report magnitude as two live lines: the fast API view first
(repos, tags — a rough size up front), the slow fs walk beneath
it (repos, tags, revisions, blobs, uploads, layer links, GiB
last). The api line carries the running fs-minus-API delta in
braces — negative while the walk counts up, converging on the
skew. The API has no endpoints for revisions, blobs, uploads,
or layer links, so those stay fs-side. Read-only and
verdict-free: the live block is the display, nothing reprints
it. Needs the filestore proof (KPR_REGISTRY_CONFIG names a
config with a filesystem storage root). Point-in-time on a live
registry.`,
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
	APIRepos   int   `json:"api_repos"`
	APITags    int   `json:"api_tags"`
}

// gib renders bytes as fractional GiB with two decimals: exact
// bytes stay in --json, humans get a sense of scale.
func gib(b int64) string {
	return fmt.Sprintf("%.2f GiB", float64(b)/1024/1024/1024)
}

// fsWaiting holds the fs line while the API view runs first: the
// walk it reports on hasn't started yet.
const fsWaiting = "fs:  walk pending"

// apiLine renders the API view: repos and tags the catalog names.
// With delta it appends the running fs-minus-API gap in braces —
// negative while the walk counts up, converging on the skew. The
// gap is information, not a verdict: a live registry shifts under
// both walks, and empty-but-listed repos legitimately differ.
func apiLine(api backfill.CatalogReport, delta bool, fs registryfs.Report) string {
	s := fmt.Sprintf("api: %d repos, %d tags", api.Repos, api.Tags)
	if !delta {
		return s
	}
	return fmt.Sprintf("%s (Δ %+d repos, %+d tags)", s, fs.Repos-api.Repos, fs.Tags-api.Tags)
}

// fsLine renders the fs walk: object counts first, byte scale last
// — blobs are many, bytes are one number of a different kind. The
// label pads to the api width so values start in one column.
func fsLine(rep registryfs.Report) string {
	return fmt.Sprintf("fs:  %d repos, %d tags, %d revisions, %d blobs, %d uploads, %d layer links, %s",
		rep.Repos, rep.Tags, rep.Revisions, rep.Blobs, rep.Uploads, rep.LayerLinks, gib(rep.BlobBytes))
}

// runRegistryAnalyze proves the filestore, runs the fast API view
// first (rough size up front), then the slow fs walk beneath it,
// both repainting one two-line block — the block is the display,
// nothing reprints it. The fs walker takes the token, never a
// bare path. The API pass reuses backfill's enumeration port. A
// refused API walk fails the command: a comparison against an
// unknown API view misleads.
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
		live.tickTwo(apiLine(running, false, registryfs.Report{}), fsWaiting)
	})
	if err != nil {
		return err
	}
	rep, err := registryfs.Analyze(fsStore, func(running registryfs.Report) {
		live.tickTwo(apiLine(api, true, running), fsLine(running))
	})
	if err != nil {
		return err
	}
	live.doneTwo(apiLine(api, true, rep), fsLine(rep))
	if asJSON {
		return json.NewEncoder(w).Encode(analyzeJSON{
			Repos: rep.Repos, Tags: rep.Tags, Revisions: rep.Revisions,
			Blobs: rep.Blobs, BlobBytes: rep.BlobBytes,
			Uploads: rep.Uploads, LayerLinks: rep.LayerLinks,
			APIRepos: api.Repos, APITags: api.Tags,
		})
	}
	return nil
}

func init() {
	registryAnalyzeCmd.Flags().BoolVar(&registryAnalyzeJSON, "json", false, "Emit the report as JSON for scripts")
	registryCmd.AddCommand(registryAnalyzeCmd)
	RootCmd.AddCommand(registryCmd)
}
