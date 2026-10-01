package proof

import (
	"fmt"
	"testing"
)

// A minted generation names itself: the token carries which
// write earned it, for loud output. If this fails, freshness
// stopped naming its generation.
func TestMintedGenerationNamesGen(t *testing.T) {
	if got := MintedGeneration("0193aBc").Gen(); got != "0193aBc" {
		t.Errorf("fresh gen = %q, want the minted one", got)
	}
}

// The zero value is nothing: without a mint there is no
// freshness. If this fails, liveness can be minted from thin
// air.
func TestFreshGenerationZeroIsNothing(t *testing.T) {
	var f FreshGeneration
	if f != nil {
		t.Errorf("zero FreshGeneration = %v, want nil", f)
	}
}

// The mint site names its write beside the Write and Verify it
// just performed — the call belongs to the minter and nowhere
// else. If this fails, the token stopped reading as a receipt.
func ExampleMintedGeneration() {
	gen := "0193aBc" // NewGen, Write, Verify read it back
	collectIfFreshGeneration(MintedGeneration(gen))
	// Output: collecting under generation 0193aBc
}

func collectIfFreshGeneration(f FreshGeneration) {
	fmt.Println("collecting under generation " + f.Gen())
}
