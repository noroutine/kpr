package cli

import (
	"strings"
	"testing"
)

// Unlock against a registry that doesn't serve the staged store
// refuses: intent never opens without proof, however reachable the
// API. If this fails, a remote kpr unlocks against nothing.
func TestUnlockRefusesUnsharedStore(t *testing.T) {
	root := t.TempDir()
	srv := serveRegistry(t, root, true)
	defer srv.Close()
	dir := t.TempDir()
	cfg := stageGCStore(t, t.TempDir())

	setUnlockConfig(t, cfg)
	_, err := runLockCmd(t, dir, srv.URL, unlockCmd)
	if err == nil || !strings.Contains(err.Error(), "does not share") {
		t.Fatalf("unlock refusal = %v, want the no-shared-store cause", err)
	}
}
