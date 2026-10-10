package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// This file is the joint home of `plan add` and `plan remove`:
// the reportStage harness both (and plan discard's test) stand
// on, plus the tests that exercise the pair together (dead-store
// refusal, end-to-end tails). Single-command tests live with
// their commands in plan_add_test and plan_remove_test; what
// cannot be attributed to one command stays here, not duplicated
// in both.

// reportStage tracks one scratch row: just enough to render a count
// against. Behavior lives in keeper; here only the messages.
func reportStage() *store.MemStore {
	s := store.NewMemStore()
	_ = s.Record(cliCtx(), policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a", PushedAt: cliNow})
	return s
}

// Edits against dead state fail naming redis: no evaluation without
// rows. If this fails, an outage edits the empty world.
func TestPlanEditsOnDeadStoreFail(t *testing.T) {
	if err := runPlanAdd(cliCtx(), io.Discard, deadStore{}, []string{"app:*"}); err == nil {
		t.Error("plan add on dead store succeeded, want an error")
	}
	if err := runPlanRemove(cliCtx(), io.Discard, deadStore{}, []string{"app:*"}); err == nil {
		t.Error("plan remove on dead store succeeded, want an error")
	}
}

// Command tails run the real RunE against a file backend: add marks
// by pattern, remove unmarks, refused patterns refuse. If any fail,
// the command is unwired (flags, deps, or close).
func TestPlanEditTailsRunAgainstFileBackend(t *testing.T) {
	dir := t.TempDir()
	srv := serveRegistry(t, t.TempDir(), false)
	defer srv.Close()
	// One tracked row to mark: the file store starts empty.
	func() {
		t.Setenv(config.EnvStore, "file")
		t.Setenv(config.EnvStoreDir, dir)
		cfg := config.NewBuilder().FromEnv().Build()
		backend, storeDir := resolveTestBackend(t)
		s, err := deps.OpenStore(cfg, backend, storeDir)
		if err != nil {
			t.Fatalf("open file store: %v", err)
		}
		defer func() { _ = s.Close() }()
		if err := s.Record(context.Background(), policy.Row{Repo: "app", Tag: "v1",
			Digest: "sha256:a", PushedAt: cliNow}); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}()
	if out, err := runCmdWithArgs(t, dir, srv.URL, planAddCmd, []string{"app:*"}); err != nil {
		t.Fatalf("plan add: %v", err)
	} else if !strings.Contains(out, "marked 1 rows due") {
		t.Errorf("add output lacks the count:\n%s", out)
	}
	if out, err := runCmdWithArgs(t, dir, srv.URL, planRemoveCmd, []string{"app:*"}); err != nil {
		t.Fatalf("plan remove: %v", err)
	} else if !strings.Contains(out, "removed 1 due marks") {
		t.Errorf("remove output lacks the count:\n%s", out)
	}
	if _, err := runCmdWithArgs(t, dir, srv.URL, planAddCmd, []string{"regex:(["}); err == nil {
		t.Error("plan add with bad pattern succeeded, want refusal")
	}
}
