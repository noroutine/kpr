package cli

import (
	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

var gcDeleteUntagged bool

var gcAcceptBlobCache bool

var gcAcceptUnfenced bool

var gcAcceptClockSkew bool

var gcAcceptRollback bool

var gcAcceptModeFlip bool

var gcNoDryRun bool

var Cmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect unreferenced registry blobs",
	Long: `Run the stock registry garbage-collect against the shared store.

Dry-run by default; --no-dry-run collects for real. Probes the
registry, proves the shared store, then collects — each risk
refuses unless overridden with its own --accept-* flag.`,
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
		return gc.Run(cmd.Context(), out, gc.Deps{
			// One store wearing all four hats (lock, recorder,
			// identity, rows), the whole store behind them, and
			// the registry API. Reporter, fence, and clock stay
			// unset — the run renders and resolves them.
			Lock: d.Store, Rec: d.Store, Ids: d.Store, Rows: d.Store,
			API: d.Reg, Store: d.Store,
		}, gc.Options{
			DeleteUntagged: gcDeleteUntagged,
			Armed:          armedRun,
		}, gcAccepts(armedRun))
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
func gcAccepts(armed proof.ArmedRun) gc.Accepts {
	return gc.Accepts{
		Cache:     proof.Force(armed, gcAcceptBlobCache),
		Fence:     proof.Force(armed, gcAcceptUnfenced),
		ClockSkew: proof.Force(armed, gcAcceptClockSkew),
		Rollback:  proof.Force(armed, gcAcceptRollback),
		ModeFlip:  proof.Force(armed, gcAcceptModeFlip),
	}
}
