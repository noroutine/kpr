package proof

// ArmedRun is human intent to mutate, earned for one run. Sealed
// like every evidence: the only inhabitants come from the
// constructors below, each naming the boundary that granted it.
// The adapter reports intent by calling a constructor — it
// cannot produce intent from nothing, and the zero value is just
// nil.
type ArmedRun interface {
	// Source reports where the arming came from: "flag" or "env".
	// For loud output only — never for branching. One concern
	// per source, each constructed where it was read.
	Source() string

	sealed()
}

// armedRun is the sole implementation.
type armedRun struct {
	source string
}

func (a armedRun) Source() string { return a.source }
func (a armedRun) sealed()        {}

// armedFromFlag builds intent from an explicit --no-dry-run.
// Unexported on purpose: intent is produced by the Arm prover
// below, never crafted at a call site.
func armedFromFlag() ArmedRun { return armedRun{source: "flag"} }

// armedFromEnv builds intent from KPR_*_NO_DRY_RUN=true. Same
// rule: produced, not crafted.
func armedFromEnv() ArmedRun { return armedRun{source: "env"} }

// Unarmed reports the absence of the proof: dry-run is no
// ArmedRun, never a boolean of its own. Commands share this
// instead of comparing to nil at scattered call sites.
func Unarmed(a ArmedRun) bool { return a == nil }

// Arm evaluates human intent at the adapter boundary: an
// explicit flag produces from the flag, the env arming produces
// from the env, anything else produces nothing — dry-run is the
// absence of the proof, never a boolean of its own.
//
// The adapter reads its own sources (cobra flag vars, config
// env) and passes the readings; precedence and producing stay
// here, beside the proof they produce. Callers derive the run
// mode from the result — an unwired flag previews instead of
// arming, loudly, by construction.
func Arm(flag, env bool) ArmedRun {
	switch {
	case flag:
		return armedFromFlag()
	case env:
		return armedFromEnv()
	default:
		return nil
	}
}
