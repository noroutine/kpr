package proof

import (
	"fmt"
	"testing"
)

// Acceptance is granted, never assumed: an --accept-* flag on an
// armed run proceeds, anything else does not compile. If this
// fails, accepted risk stopped meaning leave.
func TestAcceptedRiskNeedsArmed(t *testing.T) {
	arm := armedFromFlag()
	collectWritable(acceptedRiskFromFlag(arm))
}

// The zero value is nothing: without the flag there is no
// acceptance. If this fails, risk can be accepted from thin air.
func TestAcceptedRiskZeroIsNothing(t *testing.T) {
	var f AcceptedRisk
	if f != nil {
		t.Errorf("zero AcceptedRisk = %v, want nil", f)
	}
}

// Leave needs both intent and the flag: a flag on a preview,
// or arming without the flag, mints nothing. If this fails,
// acceptance stopped composing with intent.
func TestForceNeedsArmedAndFlag(t *testing.T) {
	armed := Arm(true, false)
	if f := Force(armed, true); f == nil {
		t.Error("Force(armed, true) = nil, want leave")
	}
	if f := Force(armed, false); f != nil {
		t.Errorf("Force(armed, false) = %v, want nil", f)
	}
	if f := Force(nil, true); f != nil {
		t.Errorf("Force(nil, true) = %v, want nil", f)
	}
	if f := Force(nil, false); f != nil {
		t.Errorf("Force(nil, false) = %v, want nil", f)
	}
}

// Evidence composes in the signature: the caller proves intent
// (ArmedRun), derives acceptance (AcceptedRisk), and the stage
// takes only the acceptance. If this fails, the chain stopped
// reading left to right.
func ExampleForce() {
	collectWritable(acceptedRiskFromFlag(armedFromFlag()))
	// Output: collecting from writable
}

func collectWritable(AcceptedRisk) {
	fmt.Println("collecting from writable")
}
