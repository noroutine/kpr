package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
)

// runLockCmd executes one lock command with the file backend rooted
// at dir, talking to the fake registry at regURL. It drives the
// subcommand directly: the package Execute() exits the process on
// error, which would kill the test binary on the refusal paths this
// file asserts. The URL pin matters: openDeps resolves the real
// default (localhost:5000) otherwise, and the test would prove
// against the dev stack instead of the staged store.
func runLockCmd(t *testing.T, dir, regURL string, target *cobra.Command) (string, error) {
	t.Helper()
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, dir)
	t.Setenv(config.EnvRegistryURL, regURL)
	var buf bytes.Buffer
	target.SetOut(&buf)
	defer target.SetOut(nil)
	// Production Execute() always installs a context; a bare RunE
	// leaves nil, and the registry client refuses it.
	target.SetContext(context.Background())
	err := target.RunE(target, nil)
	return buf.String(), err
}

// setUnlockConfig points the unlock command at cfg for one test:
// the --config flag var persists on the package, so every unlock
// test sets it explicitly instead of inheriting a stale path.
func setUnlockConfig(t *testing.T, cfg string) {
	t.Helper()
	if err := unlockCmd.Flags().Set("config", cfg); err != nil {
		t.Fatalf("set --config: %v", err)
	}
}

// Lock drops the marker and unlock re-proves it: the round trip
// opens writes, revokes them, and says both out loud. Unlock proves
// against the staged shared store (the same file-backed fake the gc
// tests prove against), so a passing round trip means proof-gated
// intent end to end. If this fails, the operator's allow/deny lever
// is disconnected from the proof.
func TestLockUnlockRoundTrip(t *testing.T) {
	root := t.TempDir()
	srv := serveRegistry(t, root, true)
	defer srv.Close()
	cfg := stageGCStore(t, root)
	dir := t.TempDir()

	setUnlockConfig(t, cfg)
	out, err := runLockCmd(t, dir, srv.URL, unlockCmd)
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if !strings.Contains(out, "store unlocked: shared store proven via") {
		t.Fatalf("unlock output names no proof:\n%s", out)
	}
	s, err := OpenStore(config.NewBuilder().FromEnv().Build())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if ok, err := s.IsUnlocked(context.Background()); err != nil || !ok {
		t.Fatalf("post-unlock IsUnlocked = (%v, %v), want (true, nil)", ok, err)
	}
	_ = s.Close()

	out, err = runLockCmd(t, dir, srv.URL, lockCmd)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if !strings.Contains(out, "store locked") {
		t.Fatalf("lock output confirms nothing:\n%s", out)
	}
}

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
