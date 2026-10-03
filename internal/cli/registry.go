package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

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
	Long: `Walk the registry's filesystem store once and report magnitude:
repos (fs-side catalog size), tags, revisions, blobs plus bytes,
uploads, layer links — then walk the API catalog for the same two
numbers it can see (repos, tags) and print the comparison. The API
has no endpoints for revisions, blobs, uploads, or layer links,
so those stay fs-side. Read-only and verdict-free. Needs the
filestore proof (KPR_REGISTRY_CONFIG names a config with a
filesystem storage root). Counters repaint live on a terminal;
point-in-time on a live registry.`,
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

// runRegistryAnalyze proves the filestore, walks the fs with live
// counters, walks the API catalog the same way, and prints both
// plus the comparison. The fs walker takes the token, never a
// bare path. The API pass reuses backfill's enumeration port and
// reports into the same live line. A refused API walk fails the
// command: a comparison against an unknown API view misleads.
func runRegistryAnalyze(ctx context.Context, w io.Writer, configPath string, reg backfill.Registry, asJSON bool) error {
	fsStore, err := proof.ProveFilesystemStore(configPath)
	if err != nil {
		return err
	}
	live := newLiveLines(w)
	rep, err := registryfs.Analyze(fsStore, func(running registryfs.Report) {
		live.tick(fmt.Sprintf("analyze fs: %d repos, %d tags, %d revisions, %d blobs",
			running.Repos, running.Tags, running.Revisions, running.Blobs))
	})
	if err != nil {
		return err
	}
	live.done(fmt.Sprintf("analyze fs: %d repos, %d tags, %d revisions, %d blobs",
		rep.Repos, rep.Tags, rep.Revisions, rep.Blobs))
	api, err := backfill.ScanCatalog(ctx, io.Discard, reg, func(running backfill.CatalogReport) {
		live.tick(fmt.Sprintf("analyze api: %d repos, %d tags", running.Repos, running.Tags))
	})
	if err != nil {
		return err
	}
	live.done(fmt.Sprintf("analyze api: %d repos, %d tags", api.Repos, api.Tags))
	return renderAnalyze(w, rep, api, asJSON)
}

// renderAnalyze prints the fs table, the API view, and the delta.
// Deltas are information, not verdicts: a live registry shifts
// under both walks, and empty-but-listed repos legitimately
// differ.
func renderAnalyze(w io.Writer, rep registryfs.Report, api backfill.CatalogReport, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(analyzeJSON{
			Repos: rep.Repos, Tags: rep.Tags, Revisions: rep.Revisions,
			Blobs: rep.Blobs, BlobBytes: rep.BlobBytes,
			Uploads: rep.Uploads, LayerLinks: rep.LayerLinks,
			APIRepos: api.Repos, APITags: api.Tags,
		})
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "REPOS\tTAGS\tREVISIONS\tBLOBS\tBLOB_BYTES\tUPLOADS\tLAYER_LINKS"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
		rep.Repos, rep.Tags, rep.Revisions, rep.Blobs, rep.BlobBytes, rep.Uploads, rep.LayerLinks); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "api: %d repos, %d tags (fs delta %+d repos, %+d tags)\n",
		api.Repos, api.Tags, api.Repos-rep.Repos, api.Tags-rep.Tags)
	return err
}

func init() {
	registryAnalyzeCmd.Flags().BoolVar(&registryAnalyzeJSON, "json", false, "Emit the report as JSON for scripts")
	registryCmd.AddCommand(registryAnalyzeCmd)
	RootCmd.AddCommand(registryCmd)
}
