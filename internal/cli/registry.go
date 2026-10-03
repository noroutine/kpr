package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registryfs"
)

var registryAnalyzeJSON bool

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Inspect the distribution registry itself",
}

var registryAnalyzeCmd = &cobra.Command{
	Use:   "analyze",
	Short: "Report registry store magnitude",
	Long: `Walk the registry's filesystem store once and report magnitude:
repos (fs-side catalog size), tags, revisions, blobs plus bytes,
uploads, layer links. Read-only and verdict-free: gaps between
the counts are raw numbers for later detectors, never judgments.
Needs the filestore proof (KPR_REGISTRY_CONFIG names a config
with a filesystem storage root); anything else refuses before
any walk. Point-in-time on a live registry.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// No openDeps: analyze reads neither the state store nor
		// the registry API — a down redis must not refuse a
		// read-only fs walk. Same shape as `env`.
		cfg := config.NewBuilder().FromEnv().Build()
		return runRegistryAnalyze(cmd.OutOrStdout(), cfg.RegistryConfig, registryAnalyzeJSON)
	},
}

// analyzeJSON is the piped shape of a magnitude report.
type analyzeJSON struct {
	Repos      int   `json:"repos"`
	Tags       int   `json:"tags"`
	Revisions  int   `json:"revisions"`
	Blobs      int   `json:"blobs"`
	BlobBytes  int64 `json:"blob_bytes"`
	Uploads    int   `json:"uploads"`
	LayerLinks int   `json:"layer_links"`
}

// runRegistryAnalyze proves the filestore and prints one walk's
// magnitude: columns for humans, JSON for scripts. The walker
// takes the token, never a bare path.
func runRegistryAnalyze(w io.Writer, configPath string, asJSON bool) error {
	fsStore, err := proof.ProveFilesystemStore(configPath)
	if err != nil {
		return err
	}
	rep, err := registryfs.Analyze(fsStore)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(w).Encode(analyzeJSON{
			Repos: rep.Repos, Tags: rep.Tags, Revisions: rep.Revisions,
			Blobs: rep.Blobs, BlobBytes: rep.BlobBytes,
			Uploads: rep.Uploads, LayerLinks: rep.LayerLinks,
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
	return tw.Flush()
}

func init() {
	registryAnalyzeCmd.Flags().BoolVar(&registryAnalyzeJSON, "json", false, "Emit the report as JSON for scripts")
	registryCmd.AddCommand(registryAnalyzeCmd)
	RootCmd.AddCommand(registryCmd)
}
