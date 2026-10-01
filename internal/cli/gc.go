package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/registry"
)

// registryBinPath is the stock registry binary gc shells out to. The
// image COPYs it from the same registry:3 the stack runs, so collector
// and store versions match by construction.
var registryBinPath = "/bin/registry"

var gcConfigPath string

var gcDeleteUntagged bool

var gcForce bool

var gcNoDryRun bool

// renderGCEvent voices the lifecycle loud: probe verdicts, collector
// start (pid, so a long mark phase is visibly alive), post-probe, and
// the flip banner. Collector lines stream raw alongside.
func renderGCEvent(w io.Writer, dryRun bool) gc.Reporter {
	return func(e gc.Event) {
		switch e.Stage {
		case gc.StagePreProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry is %s\n", strings.ToUpper(e.Message))
		case gc.StageStarted:
			if e.PID != 0 {
				_, _ = fmt.Fprintf(w, "collector started (pid %d)%s\n", e.PID, drySuffix(dryRun))
			}
		case gc.StagePostProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry still %s\n", strings.ToUpper(e.Message))
		case gc.StageModeFlip:
			_, _ = fmt.Fprintf(w, "WARNING: registry flipped %s mid-run\n", e.Message)
		case gc.StageFailure:
			_, _ = fmt.Fprintf(w, "collector failed: %s\n", e.Error)
		}
	}
}

func drySuffix(dryRun bool) string {
	if dryRun {
		return " — dry-run, nothing will be deleted"
	}
	return ""
}

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect unreferenced registry blobs",
	Long: `Run the stock registry garbage-collect against the shared store,
streaming its output and reporting each stage, in dry-run mode unless
--no-dry-run (or KPR_CLI_NO_DRY_RUN=true), which collects for real.
The sentinel probes the registry first (readonly collects, a
real run on writable refuses unless --force — flip
storage.maintenance.readonly and restart it instead — a preview on
writable proceeds warned, inconclusive always refuses), then proves
the store shared with a fresh generation it reads back through the
API. After the
collect the sentinel re-probes: a mode flip mid-run is loud but
never a panic — it fails the run unless --force (which presumes you
know). Flipping readonly stays with the operator — this command
never rewrites registry config. The store starts locked (fresh
stores included): a locked run refuses before probing — 'kpr
unlock' proves the shared store and opens writes, 'kpr lock'
revokes.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		cfg := d.cfg
		out := cmd.OutOrStdout()
		dryRun := !gcNoDryRun && !cfg.CLINoDryRun
		return gc.Run(cmd.Context(), out, gc.ProbeRegistry, d.store, gc.RunCollector, registry.NewClient(cfg.RegistryURL), cfg.RegistryURL, gcConfigPath, registryBinPath, d.store, gc.Options{
			DeleteUntagged: gcDeleteUntagged,
			Force:          gcForce,
			DryRun:         dryRun,
			Report:         renderGCEvent(out, dryRun),
		})
	},
}

func init() {
	gcCmd.Flags().StringVar(&gcConfigPath, "config", "/etc/distribution/config.yml", "Registry config file (shared store paths come from it)")
	gcCmd.Flags().BoolVar(&gcDeleteUntagged, "delete-untagged", false, "Also drop orphaned manifests (same flag as registry garbage-collect)")
	gcCmd.Flags().BoolVar(&gcForce, "force", false, "Collect even when the sentinel finds the registry writable (presumes you know)")
	gcCmd.Flags().BoolVar(&gcNoDryRun, "no-dry-run", false, "Collect for real (default previews with the collector's --dry-run)")
	RootCmd.AddCommand(gcCmd)
}
