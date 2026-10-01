package proof

// FreshGeneration is a generation this run minted, wrote, and
// read back: liveness + currency + same-store in one token.
// Sealed like every evidence: the only inhabitant comes from
// MintedGeneration, and the zero value is nil.
//
// Kept honest: unlike the Prover, no re-check stands behind
// this token — its strength is call-site discipline.
// MintedGeneration is called once, at the mint site, beside the
// Write and Verify it names. A FreshGeneration from anywhere
// else is a lie the seal cannot catch; review the call sites,
// there is one per minter.
type FreshGeneration interface {
	// Gen is the minted generation, for loud output only —
	// never for branching.
	Gen() string

	sealed()
}

type freshGeneration struct{ gen string }

func (f freshGeneration) Gen() string { return f.gen }
func (f freshGeneration) sealed()     {}

// MintedGeneration names the generation just minted, written,
// and read back. Call it at the mint site and nowhere else.
func MintedGeneration(gen string) FreshGeneration { return freshGeneration{gen: gen} }
