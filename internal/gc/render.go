package gc

import (
	"fmt"
	"io"
	"strings"

	"nrtn.dev/catalyst/kpr/internal/event"
)

// renderEvent is the default reporter: voices the lifecycle loud
// (probe verdicts, collector start with pid so a long mark phase
// is visibly alive, post-probe, the flip banner, the fence lines
// around an armed collect) while collector lines stream raw
// alongside. Run installs it when Deps.Report is nil — an
// injected reporter always wins, so tests observe stages
// silently and production never threads one through.
func renderEvent(w io.Writer, dryRun bool) event.Reporter {
	return func(e event.Event) {
		switch e.Stage {
		case stagePreProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry is %s\n", strings.ToUpper(e.Message))
		case stageHoldEngage:
			_, _ = fmt.Fprintf(w, "HOLD engaged: manifest writes wait out the armed collect\n")
		case stageHoldRelease:
			_, _ = fmt.Fprintf(w, "HOLD released: manifest writes flow again\n")
		case stageStarted:
			if e.PID != 0 {
				_, _ = fmt.Fprintf(w, "collector started (pid %d)%s\n", e.PID, drySuffix(dryRun))
			}
		case stagePostProbe:
			_, _ = fmt.Fprintf(w, "sentinel: registry still %s\n", strings.ToUpper(e.Message))
		case stageModeFlip:
			_, _ = fmt.Fprintf(w, "WARNING: registry flipped %s mid-run\n", e.Message)
		case stageFailure:
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
