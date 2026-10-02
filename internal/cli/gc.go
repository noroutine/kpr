package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/edge"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// registryBinPath is the stock registry binary gc shells out to. The
// image COPYs it from the same registry:3 the stack runs, so collector
// and store versions match by construction.
var registryBinPath = "/bin/registry"

var gcConfigPath string

var gcDeleteUntagged bool

var gcForce bool

var gcAcceptBlobCache bool

var gcAcceptUnfenced bool

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

// fenceForBackend wires the HOLD lease: the file backend shares
// the lease dir with the edge, anything else runs unfenced with
// the warning said out loud. Previews stay silent either way —
// nothing is deleted, so nothing holds.
func fenceForBackend(backend, dir string, backendErr error, dryRun bool, out io.Writer) gc.Fencer {
	if backendErr == nil && backend == "file" {
		return edge.HoldFile{Dir: dir}
	}
	if !dryRun {
		_, _ = fmt.Fprintln(out, "Warning: proxy HOLD fence unavailable without a shared file store — armed collect runs unfenced")
	}
	return nil
}

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect unreferenced registry blobs",
	Long: `Run the stock registry garbage-collect against the shared store,
streaming its output and reporting each stage, in dry-run mode unless
--no-dry-run (or KPR_CLI_NO_DRY_RUN=true), which collects for real.
The sentinel probes the registry first: stopped (readonly) takes
the classic offline collect; serving (writable) takes the online
path — an armed run clears the blob cache and the gateway fence
up front (each overridable with --accept-blob-cache /
--accept-unfenced), a preview prints the same checklist and
proceeds warned, inconclusive always refuses. Then gc proves
the store shared with a fresh generation it reads back through the
API. After the
collect the sentinel re-probes: a mode flip mid-run is loud but
never a panic — it fails the run unless --force (which presumes you
know). Flipping readonly stays with the operator — this command
never rewrites registry config. The store starts locked (fresh
stores included): a locked run refuses before probing — 'kpr
store unlock' proves the shared store and opens writes, 'kpr store lock'
revokes.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := openDeps()
		if err != nil {
			return err
		}
		defer d.close()
		cfg := d.cfg
		out := cmd.OutOrStdout()
		// The run mode flows from the mint: dry-run is the absence
		// of Armed. The adapter feeds raw readings (flag var,
		// config value); minting stays in proof.
		armedRun := proof.Arm(gcNoDryRun, cfg.CLINoDryRun)
		dryRun := gcDryRun(armedRun)
		// Each online risk mints its own acceptance from the same
		// armed run: --force keeps its other jobs (clock skew,
		// restored lineage, post-run flip), the writable collect
		// answers to these two.
		cacheAccept := proof.Force(armedRun, gcAcceptBlobCache)
		fenceAccept := proof.Force(armedRun, gcAcceptUnfenced)
		backend, dir, berr := resolveStoreBackend()
		fence := fenceForBackend(backend, dir, berr, dryRun, out)
		return gc.Run(cmd.Context(), out, gc.ProbeRegistry, d.store, gc.RunCollector, d.reg, cfg.RegistryURL, gcConfigPath, registryBinPath, d.store, d.store, d.store, clockSource(d.cfg), d.cfg.TimeServer, gc.Options{
			DeleteUntagged: gcDeleteUntagged,
			Force:          gcForce,
			DryRun:         dryRun,
			Report:         renderGCEvent(out, dryRun),
			EdgeAddr:       cfg.EdgeAddr,
			Fence:          fence,
		}, cacheAccept, fenceAccept)
	},
}

func init() {
	gcCmd.Flags().StringVar(&gcConfigPath, "config", "/etc/distribution/config.yml", "Registry config file (shared store paths come from it)")
	gcCmd.Flags().BoolVar(&gcDeleteUntagged, "delete-untagged", false, "Also drop orphaned manifests (same flag as registry garbage-collect)")
	gcCmd.Flags().BoolVar(&gcForce, "force", false, "Accept clock skew, restored lineage, and post-run mode flips (presumes you know)")
	gcCmd.Flags().BoolVar(&gcAcceptBlobCache, "accept-blob-cache", false, "Collect with a blobdescriptor cache configured (deletes stay vouched until restart)")
	gcCmd.Flags().BoolVar(&gcAcceptUnfenced, "accept-unfenced", false, "Collect without the gateway HOLD fence (a push mid-collect corrupts)")
	gcCmd.Flags().BoolVar(&gcNoDryRun, "no-dry-run", false, "Collect for real (default previews with the collector's --dry-run)")
	RootCmd.AddCommand(gcCmd)
}

// gcDryRun reads the mode off the mint: dry-run is the absence of
// Armed, never a second flag. If this fails, gc's preview/collect
// split answers to something other than the mint.
func gcDryRun(a proof.ArmedRun) bool { return a == nil }
