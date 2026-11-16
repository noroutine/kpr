//go:build unix

package resources

import (
	"path/filepath"
	"syscall"
	"testing"
)

// A fifo named .yaml reads as nothing, not a hang: the loader
// only opens regular files. If this fails, a stray pipe in the
// folder stalls the boot on open.
func TestLoadSkipsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.yaml"), 0o600); err != nil {
		t.Skipf("no fifo here: %v", err)
	}
	snap, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with a fifo = %v, want clean", err)
	}
	if len(snap.Bundles) != 0 {
		t.Errorf("bundles = %d, want 0 (pipes never open)", len(snap.Bundles))
	}
}
