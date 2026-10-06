package gc

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/fencing"
	gcrun "nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var gcDeleteUntagged bool

var gcAcceptBlobCache bool

var gcAcceptUnfenced bool

var gcAcceptClockSkew bool

var gcAcceptRollback bool

var gcAcceptModeFlip bool

var gcNoDryRun bool

// renderGCEvent voices the lifecycle loud: probe verdicts, collector
// start (pid, so a long mark phase is visibly alive), post-probe,
// the flip banner, and the fence lines the run voices around an
// armed collect. Collector lines stream raw alongside.
func renderGCEvent(w io.Writer, dryRun bool) fence.Reporter {
	return func(e fence.Event) {
		switch e.Stage {
		case gcrun.StagePreProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry is %s\n", strings.ToUpper(e.Message))
		case gcrun.StageHoldEngage:
			_, _ = fmt.Fprintf(w, "HOLD engaged: manifest writes wait out the armed collect\n")
		case gcrun.StageHoldRelease:
			_, _ = fmt.Fprintf(w, "HOLD released: manifest writes flow again\n")
		case gcrun.StageStarted:
			if e.PID != 0 {
				_, _ = fmt.Fprintf(w, "collector started (pid %d)%s\n", e.PID, drySuffix(dryRun))
			}
		case gcrun.StagePostProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry still %s\n", strings.ToUpper(e.Message))
		case gcrun.StageModeFlip:
			_, _ = fmt.Fprintf(w, "WARNING: registry flipped %s mid-run\n", e.Message)
		case gcrun.StageFailure:
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

// newFenceControl builds the run's fence adapter: lease file
// plus ring announcements over the run's store. It travels into
// gc as a factory — the use case owns the fencing decision,
// the edge owns the adapter.
func newFenceControl(st fence.GateStore) func(string) fence.Controller {
	return func(dir string) fence.Controller {
		return fencing.Control{HoldFile: store.HoldFile{Dir: dir}, Store: st}
	}
}

var Cmd = &cobra.Command{
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
proceeds warned, inconclusive always refuses. Three further risks
each refuse with their own --accept-* flag and no umbrella:
clock skew past tolerance (--accept-clock-skew), a restored older
generation (--accept-rollback), and a registry mode flip mid-run
(--accept-mode-flip). Then gc proves
the store shared with a fresh generation it reads back through the
API. After the
collect the sentinel re-probes: a mode flip mid-run is loud but
never a panic — it fails the run unless --accept-mode-flip
(which presumes you verified pulls after the run). Flipping readonly stays with the operator — this command
never rewrites registry config. The store starts locked (fresh
stores included): a locked run refuses before probing — 'kpr
store unlock' proves the shared store and opens writes, 'kpr store lock'
revokes.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := deps.OpenDeps()
		if err != nil {
			return err
		}
		defer d.Close()
		cfg := d.Cfg
		out := cmd.OutOrStdout()
		// The run mode flows from the mint: dry-run is the absence
		// of Armed. The adapter feeds raw readings (flag var,
		// config value); minting stays in proof.
		armedRun := proof.Arm(gcNoDryRun, cfg.CLINoDryRun)
		dryRun := GcDryRun(armedRun)
		accepts := gcAccepts(armedRun)
		backend, dir, berr := deps.ResolveStoreBackend()
		report := renderGCEvent(out, dryRun)
		fence := gcrun.FenceForBackend(backend, dir, newFenceControl(d.Store), berr, dryRun, out)
		return gcrun.Run(cmd.Context(), out, wirePorts(d, report, fence), gcrun.Options{
			DeleteUntagged: gcDeleteUntagged,
			DryRun:         dryRun,
		}, accepts)
	},
}

func init() {
	Cmd.Flags().BoolVar(&gcDeleteUntagged, "delete-untagged", false, "Also drop orphaned manifests (same flag as registry garbage-collect)")
	Cmd.Flags().BoolVar(&gcAcceptBlobCache, "accept-blob-cache", false, "Collect with a blobdescriptor cache configured (deletes stay vouched until restart)")
	Cmd.Flags().BoolVar(&gcAcceptUnfenced, "accept-unfenced", false, "Collect without the gateway HOLD fence (a push mid-collect corrupts)")
	Cmd.Flags().BoolVar(&gcAcceptClockSkew, "accept-clock-skew", false, "Collect with clock skew past tolerance (mint timestamps may misorder)")
	Cmd.Flags().BoolVar(&gcAcceptRollback, "accept-rollback", false, "Collect against a restored older generation (a rollback may have resurrected blobs)")
	Cmd.Flags().BoolVar(&gcAcceptModeFlip, "accept-mode-flip", false, "Trust a collect the registry flipped writable mid-run (writes may have raced the mark phase)")
	Cmd.Flags().BoolVar(&gcNoDryRun, "no-dry-run", false, "Collect for real (default previews with the collector's --dry-run)")
}

// gcAccepts mints one acceptance per named risk from the same
// armed run: each --accept-* flag clears exactly its own gate,
// nothing else. There is no umbrella — a test below pins that no
// single flag mints the whole set. If this fails, an umbrella
// re-entered through a shared mint.
func gcAccepts(armed proof.ArmedRun) gcrun.Accepts {
	return gcrun.Accepts{
		Cache:     proof.Force(armed, gcAcceptBlobCache),
		Fence:     proof.Force(armed, gcAcceptUnfenced),
		ClockSkew: proof.Force(armed, gcAcceptClockSkew),
		Rollback:  proof.Force(armed, gcAcceptRollback),
		ModeFlip:  proof.Force(armed, gcAcceptModeFlip),
	}
}

// GcDryRun reads the mode off the mint: dry-run is the absence of
// Armed, never a second flag. Exported for commands that share
// the run-mode semantics (backfill) until they move to their own
// subpackages. If this fails, preview/collect splits answer to
// something other than the mint.
func GcDryRun(a proof.ArmedRun) bool { return a == nil }
