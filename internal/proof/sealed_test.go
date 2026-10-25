package proof

import "testing"

// Every proof type still carries its seal, executed once here: the
// witnesses are uncallable from outside this package by design, so
// only a white-box call can ever run them — and without a body
// statement (`return`), even a call would leave the cover profile
// at 0 statements. If this fails, a proof type lost its marker (or
// the report went back to punishing a design property).
func TestSealedWitnessesExecute(t *testing.T) {
	armedRun{}.sealed()
	blobCacheOff{}.sealed()
	boundedClock{}.sealed()
	filesystemStore{}.sealed()
	freshGeneration{}.sealed()
	gatewayFencing{}.sealed()
	registryReadonly{}.sealed()
	registryWritable{}.sealed()
	relativeURLs{}.sealed()
	acceptedRisk{}.sealed()
	sameStore{}.sealed()
	unlockedStore{}.sealed()
}
