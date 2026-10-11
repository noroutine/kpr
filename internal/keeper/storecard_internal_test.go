package keeper

import (
	"path/filepath"
	"syscall"
	"testing"
)

// Nonsense sizes never reach the card: zero total, or free above
// total — including the free==total edge a looser check would let
// through. If this fails, a corrupt Statfs voices petabytes as
// capacity.
func TestSaneSizes(t *testing.T) {
	for _, tc := range []struct {
		total, free uint64
		want        bool
	}{
		{0, 0, false},
		{100, 101, false},
		{100, 100, true},
		{100, 30, true},
	} {
		if got := saneSizes(tc.total, tc.free); got != tc.want {
			t.Errorf("saneSizes(%d, %d) = %v, want %v", tc.total, tc.free, got, tc.want)
		}
	}
	// The live path over a real dir stays sane: block math that
	// underflows units reads zero total and refuses.
	if total, free, ok := fsSizes(t.TempDir()); !ok || total == 0 || free > total {
		t.Errorf("fsSizes(tempdir) = %d/%d/%v, want sane sizes", total, free, ok)
	}
}

// Nonsense blocks refuse without a syscall: zero total and
// free-above-total never reach the card, whatever the filesystem
// claims. If this fails, a corrupt Statfs voices petabytes as
// capacity.
func TestSizesFromBlocksRefusesNonsense(t *testing.T) {
	if _, _, ok := sizesFromBlocks(0, 0, 4096); ok {
		t.Error("sizesFromBlocks(0 blocks) ok, want refusal")
	}
	if _, _, ok := sizesFromBlocks(100, 200, 4096); ok {
		t.Error("sizesFromBlocks(free above total) ok, want refusal")
	}
	if total, free, ok := sizesFromBlocks(100, 30, 4096); !ok || total != 409600 || free != 122880 {
		t.Errorf("sizesFromBlocks = %d/%d/%v, want 409600/122880/true", total, free, ok)
	}
}

// fsSizes voices block math, not block counts: totals are raw
// blocks times the fragment unit. If this fails, the card sizes
// block counts as bytes.
func TestFsSizesMultipliesByUnit(t *testing.T) {
	dir := t.TempDir()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	unit := fsBlockUnit(&fs)
	total, free, ok := fsSizes(dir)
	if !ok {
		t.Fatal("fsSizes refused a plain temp dir")
	}
	if total != uint64(fs.Blocks)*unit || free != uint64(fs.Bavail)*unit {
		t.Errorf("fsSizes = %d/%d, want blocks×unit %d/%d",
			total, free, uint64(fs.Blocks)*unit, uint64(fs.Bavail)*unit)
	}
}

// An unreadable mount gets no ink, not an error card: Statfs
// failure reads empty, and the card drops the size line. If this
// fails, a dead mount breaks the store card.
func TestFsStatsEmptyOnUnreadableMount(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	if got := fsStats(absent); got != "" {
		t.Errorf("fsStats(absent) = %q, want empty", got)
	}
	if _, _, ok := fsSizes(absent); ok {
		t.Error("fsSizes(absent) ok, want refusal")
	}
}
