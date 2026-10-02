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

// A locked gc refuses with the error alone: no usage screen (that
// answers flag typos, not refusals) and no "Error:" echo (Execute
// prints the message once). Drives the real root path — RunE alone
// never prints usage, so only Execute proves the silence. If this
// fails, every refusal buries its fix under a help dump.
func TestGCLockedRefusalOmitsUsage(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	var buf bytes.Buffer
	RootCmd.SetOut(&buf)
	RootCmd.SetErr(&buf)
	RootCmd.SetArgs([]string{"gc"})
	defer RootCmd.SetArgs(nil)
	defer RootCmd.SetOut(nil)
	defer RootCmd.SetErr(nil)
	err := RootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "store is locked") {
		t.Fatalf("locked gc = %v, want the locked refusal", err)
	}
	if out := buf.String(); strings.Contains(out, "Usage:") || strings.Contains(out, "Error:") {
		t.Errorf("refusal prints help or echoes:\n%s", out)
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

// Commands without state refuse naming it: one bad backend
// exercises lock's and unlock's open failures. If this fails, a
// command invents intent without a backend.
func TestLockCommandsRefuseBadBackend(t *testing.T) {
	t.Setenv(config.EnvStore, "bogus-backend")
	for _, target := range []*cobra.Command{lockCmd, unlockCmd, adoptCmd, gcCmd} {
		var buf bytes.Buffer
		target.SetOut(&buf)
		defer target.SetOut(nil)
		target.SetContext(context.Background())
		if err := target.RunE(target, nil); err == nil {
			t.Errorf("%s on bad backend succeeded, want refusal", target.Use)
		}
	}
}

// Adopt bootstraps the pairing onto silence: a fresh store facing
// an empty registry pairs to the pinned IDENT without minting. If
// this fails, the explicit ceremony cannot onboard a fresh deploy.
func TestAdoptBootstrapsSilence(t *testing.T) {
	regRoot := t.TempDir()
	srv := serveRegistry(t, regRoot, true)
	defer srv.Close()
	dir := t.TempDir()
	ident := "0193abcd-0000-7000-8000-0000000000aa"
	out, err := runCmdWithArgs(t, dir, srv.URL, adoptCmd, []string{ident})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !strings.Contains(out, "paired to "+ident) {
		t.Errorf("adopt output lacks the pairing:\n%s", out)
	}
	s, err := OpenStore(config.NewBuilder().FromEnv().Build())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()
	paired, err := s.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if paired.ID != ident {
		t.Errorf("paired = %q, want the pinned %q", paired.ID, ident)
	}
}
