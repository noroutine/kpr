//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registryfs"
)

// A repo deleted past the janitor (rm -rf under a running
// registry) leaves a tracked row both witnesses agree is gone:
// the catalog 404s it and the fs holds no manifest dir. The
// ghosts listing names it with 404 evidence while the live
// control repo stays out, and dropping the row converges the
// view. If this fails, out-of-band deletions pile up where no
// command can see them.
func TestGhostRowConverges(t *testing.T) {
	fx := NewStorageFixture(t)
	s := New(t, fx)
	s.establishLineage()
	s.Push("test/ghost", "v1", time.Hour)
	s.Push("test/live", "v1", time.Hour)

	// Out-of-band deletion: the repo dir goes, kpr is not told.
	gone := filepath.Join(fx.StorageDir(), "docker", "registry", "v2", "repositories", "test", "ghost")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("rm repo dir: %v", err)
	}

	ctx, cancel := s.ctx()
	defer cancel()
	same, err := proof.Prover{Sentinel: s.reg, Store: s.store}.Prove(ctx)
	if err != nil {
		t.Fatalf("prove same-store: %v", err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+fx.StorageDir()+"\n"), 0o600); err != nil {
		t.Fatalf("stage registry config: %v", err)
	}
	fsStore, err := proof.ProveFilesystemStore(cfg)
	if err != nil {
		t.Fatalf("prove fs: %v", err)
	}
	fsRepos, err := registryfs.ListRepos(fsStore)
	if err != nil {
		t.Fatalf("list fs repos: %v", err)
	}
	if fsRepos["test/ghost"] {
		t.Fatalf("fs still names test/ghost after rm -rf")
	}
	ghosts, conflicts, unreadable, err := keeper.ListGhosts(ctx, s.store, s.reg, fsRepos, same)
	if err != nil {
		t.Fatalf("ListGhosts: %v", err)
	}
	if len(ghosts) != 1 || ghosts[0].Row.Repo != "test/ghost" {
		t.Fatalf("ghosts = %+v, want [test/ghost:v1]", ghosts)
	}
	if got := ghosts[0].Evidence; got != "catalog 404, fs absent" {
		t.Errorf("evidence = %q, want the 404 account", got)
	}
	if len(conflicts) != 0 || len(unreadable) != 0 {
		t.Errorf("conflicts = %v unreadable = %v, want both empty", conflicts, unreadable)
	}

	// The operator path: plain row drop converges the view.
	if err := s.store.Delete(ctx, "test/ghost", "v1"); err != nil {
		t.Fatalf("drop ghost row: %v", err)
	}
	ghosts, _, _, err = keeper.ListGhosts(ctx, s.store, s.reg, fsRepos, same)
	if err != nil {
		t.Fatalf("ListGhosts after drop: %v", err)
	}
	if len(ghosts) != 0 {
		t.Errorf("ghosts after drop = %+v, want none", ghosts)
	}
}
