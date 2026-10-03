package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// The backfill command exposes its contract on the help screen:
// the glob positional, preview-by-default arming, the config it
// resolves the mount from, and the one accepted risk. If this
// fails, the flags drifted from backfill.Run.
func TestStoreBackfillHelpNamesContract(t *testing.T) {
	var buf bytes.Buffer
	RootCmd.SetOut(&buf)
	defer RootCmd.SetOut(nil)
	RootCmd.SetArgs([]string{"store", "backfill", "--help"})
	defer RootCmd.SetArgs(nil)
	// Cobra keeps parsed flag values on the shared command: without
	// this, every later `store backfill` invocation in the package
	// reprints help and succeeds.
	defer func() { _ = storeBackfillCmd.Flags().Set("help", "false") }()
	Execute()
	out := buf.String()
	for _, want := range []string{"repo-glob", "no-dry-run", "accept-rollback", "config"} {
		if !strings.Contains(out, want) {
			t.Errorf("backfill help omits %q", want)
		}
	}
}

// The backfill wrapper refuses like the other one-shots: no
// backend, no run. If this fails, the command invents rows with
// nothing behind it.
func TestStoreBackfillRefusesWithoutBackend(t *testing.T) {
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	RootCmd.SetArgs([]string{"store", "backfill"})
	defer RootCmd.SetArgs(nil)
	if err := RootCmd.Execute(); err == nil {
		t.Error("store backfill without redis succeeded, want a fast error")
	} else if !strings.Contains(err.Error(), "redis") {
		t.Errorf("store backfill error = %q, want it to name redis", err.Error())
	}
}

// An unreadable registry config fails the run before any proof:
// the mount path comes from it, so guessing is refusing.
func TestStoreBackfillRefusesWithoutRegistryConfig(t *testing.T) {
	clearStoreEnv(t)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	missing := filepath.Join(t.TempDir(), "nope.yml")
	RootCmd.SetArgs([]string{"store", "backfill", "--config", missing})
	defer RootCmd.SetArgs(nil)
	defer func() { _ = storeBackfillCmd.Flags().Set("config", "/etc/distribution/config.yml") }()
	if err := RootCmd.Execute(); err == nil {
		t.Error("store backfill without registry config succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "nope.yml") {
		t.Errorf("store backfill error = %q, want it to name the config", err.Error())
	}
}

// The glob positional reaches the run: bare and scoped invocations
// both get past the plumbing to the registry refusal behind it
// (no registry here). If this fails, the positional never arrives.
func TestStoreBackfillPassesGlobToRun(t *testing.T) {
	clearStoreEnv(t)
	dir := t.TempDir()
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, dir)
	root := t.TempDir()
	cfg := "storage:\n  filesystem:\n    rootdirectory: " + root + "\n"
	cfgPath := filepath.Join(t.TempDir(), "registry.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("stage registry config: %v", err)
	}
	if err := store.NewFileStore(dir).SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock file store: %v", err)
	}
	for _, args := range [][]string{
		{"store", "backfill", "--config", cfgPath},
		{"store", "backfill", "--config", cfgPath, "test/*"},
	} {
		RootCmd.SetArgs(args)
		defer RootCmd.SetArgs(nil)
		defer func() { _ = storeBackfillCmd.Flags().Set("config", "/etc/distribution/config.yml") }()
		if err := RootCmd.Execute(); err == nil {
			t.Errorf("store backfill %q without registry succeeded, want refusal", args)
		}
	}
}
