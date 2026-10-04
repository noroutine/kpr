//go:build darwin

package console

import "testing"

// The block unit must stay the fundamental size on macOS, where
// statfs carries no separate fragment size. If this fails, the
// capacity card is sizing fantasy storage again.
func TestFsSizesSaneOnDarwin(t *testing.T) {
	total, free, ok := fsSizes(t.TempDir())
	if !ok {
		t.Fatal("fsSizes refused a plain temp dir")
	}
	const petabyte = uint64(1) << 50
	if total >= petabyte {
		t.Errorf("total = %d bytes, want below 1 PB (wrong block unit?)", total)
	}
	if free > total {
		t.Errorf("free = %d above total = %d", free, total)
	}
}
