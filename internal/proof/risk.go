package proof

// AcceptedRisk is the operator accepting a loud refusal: the run
// names what stands in front of it, the human presumes to know
// and proceeds anyway. One flag, any gate that offers the escape
// hatch — the writable registry, the skewed clock. Sealed like
// every evidence: the only inhabitant comes from the constructor
// below, and the zero value is nil.
//
// The constructor takes ArmedRun — risk without intent is
// meaningless, and the signature is where that is said. A stage
// that needs AcceptedRisk names a caller that already proved
// intent; no check to remember, no flag to re-read. Which risk
// was accepted is said at the gate, loudly, in its own message —
// the token carries only that acceptance happened.
type AcceptedRisk interface {
	sealed()
}

// acceptedRisk is the sole implementation.
type acceptedRisk struct{}

func (acceptedRisk) sealed() {}

// acceptedRiskFromFlag builds acceptance from an explicit
// --force / --accept-* flag, granted only to an already-armed
// caller. Unexported on purpose: acceptance is produced by the
// Force prover below, never crafted at a call site.
func acceptedRiskFromFlag(ArmedRun) AcceptedRisk { return acceptedRisk{} }

// Force derives acceptance from intent: the flag on an armed run
// mints, anything else mints nothing. Force without arming is
// meaningless, and the signature is where that is said.
func Force(armed ArmedRun, flag bool) AcceptedRisk {
	if armed == nil || !flag {
		return nil
	}
	return acceptedRiskFromFlag(armed)
}
