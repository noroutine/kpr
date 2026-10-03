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
	Long: `Report magnitude: first the fast API view (repos, tags —
a rough size up front), then the slow fs walk (repos, tags,
revisions, blobs plus GiB, uploads, layer links) with live
counters, closing with the fs-vs-API delta. The API has no
endpoints for revisions, blobs, uploads, or layer links, so
those stay fs-side. Read-only and verdict-free: the live lines
are the display, no trailing table duplicates them. Needs the
filestore proof (KPR_REGISTRY_CONFIG names a config with a
filesystem storage root). Point-in-time on a live registry.`,
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

// fsCounters renders the full fs counter body: the live repaint
// and the final report share it, so the display never duplicates.
func fsCounters(rep registryfs.Report) string {
	return fmt.Sprintf("%d repos, %d tags, %d revisions, %d blobs, %s, %d uploads, %d layer links",
		rep.Repos, rep.Tags, rep.Revisions, rep.Blobs, gib(rep.BlobBytes), rep.Uploads, rep.LayerLinks)
}

// runRegistryAnalyze proves the filestore, runs the fast API view
// first (rough size up front), then the slow fs walk, closing
// with the comparison. The fs walker takes the token, never a
// bare path. The API pass reuses backfill's enumeration port and
// reports into the same live line. A refused API walk fails the
// command: a comparison against an unknown API view misleads.
func runRegistryAnalyze(ctx context.Context, w io.Writer, configPath string, reg backfill.Registry, asJSON bool) error {
	fsStore, err := proof.ProveFilesystemStore(configPath)
	if err != nil {
		return err
	}
	live := newLiveLines(w)
	api, err := backfill.ScanCatalog(ctx, io.Discard, reg, func(running backfill.CatalogReport) {
		live.tick(fmt.Sprintf("api: %d repos, %d tags", running.Repos, running.Tags))
	})
	if err != nil {
		return err
	}
	live.done(fmt.Sprintf("api: %d repos, %d tags", api.Repos, api.Tags))
	rep, err := registryfs.Analyze(fsStore, func(running registryfs.Report) {
		live.tick("fs: " + fsCounters(running))
	})
	if err != nil {
		return err
	}
	live.done("fs: " + fsCounters(rep))
	return renderAnalyze(w, rep, api, asJSON)
}

// renderAnalyze prints the final fs line, the API view, and the
// delta — the same lines the live counters already showed, so
// pipes get the numbers without repaints. Deltas are information,
// not verdicts: a live registry shifts under both walks, and
// empty-but-listed repos legitimately differ.
func renderAnalyze(w io.Writer, rep registryfs.Report, api backfill.CatalogReport, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(analyzeJSON{
			Repos: rep.Repos, Tags: rep.Tags, Revisions: rep.Revisions,
			Blobs: rep.Blobs, BlobBytes: rep.BlobBytes,
			Uploads: rep.Uploads, LayerLinks: rep.LayerLinks,
			APIRepos: api.Repos, APITags: api.Tags,
		})
	}
	if _, err := fmt.Fprintf(w, "api: %d repos, %d tags (fs delta %+d repos, %+d tags)\n",
		api.Repos, api.Tags, api.Repos-rep.Repos, api.Tags-rep.Tags); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "fs: "+fsCounters(rep))
	return err
}

func init() {
	registryAnalyzeCmd.Flags().BoolVar(&registryAnalyzeJSON, "json", false, "Emit the report as JSON for scripts")
	registryCmd.AddCommand(registryAnalyzeCmd)
	RootCmd.AddCommand(registryCmd)
}
