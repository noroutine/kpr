package gc

import "testing"

// useSeams swaps the probe/collect seams for one test and restores
// production afterwards: the per-test stubs the old Deps fields
// carried now ride the vars instead. Tests never run parallel, so
// the swap is safe.
func useSeams(t *testing.T, p Probe, c Collector) {
	t.Helper()
	oldProbe, oldCollect := probe, collect
	probe, collect = p, c
	t.Cleanup(func() { probe, collect = oldProbe, oldCollect })
}

// TestSeamsDefaultToProduction pins the seam contract: swapped only
// in tests, production always wired. A nil seam panics mid-run, so
// the default is asserted, not assumed.
func TestSeamsDefaultToProduction(t *testing.T) {
	if probe == nil || collect == nil {
		t.Fatal("probe/collect seams nil: Run would panic, production must stay wired")
	}
}
