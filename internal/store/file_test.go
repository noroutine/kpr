package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/storetest"
)

// The file store must satisfy the same contract as mem and redis:
// one suite, never a copy per backend. If this fails, the redis-less
// mode reasons differently about rows, marks, or locks.
func TestFileStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) store.Store {
		t.Helper()
		s := store.NewFileStore(t.TempDir())
		if err := s.Ping(t.Context()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		return s
	})
}

// Crash residue (abandoned temp files) and foreign files (operator
// notes) must never pollute reads: temp names carry no .json suffix
// and non-JSON is skipped. A torn .json, which our own protocol can
// never produce, refuses loudly with the path. If this fails, a
// kill -9 either resurrects as phantom rows or silently poisons All.
func TestFileStoreIgnoresResidueRefusesTorn(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if err := s.Record(ctx, policyRow("app", "v1")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rows := filepath.Join(dir, "rows", "app")
	if err := os.WriteFile(filepath.Join(rows, ".tmp-12345"), []byte(`{"half`), 0o644); err != nil {
		t.Fatalf("stage temp residue: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rows, "README.md"), []byte("operator notes"), 0o644); err != nil {
		t.Fatalf("stage foreign file: %v", err)
	}
	if all, err := s.All(ctx); err != nil || len(all) != 1 {
		t.Errorf("All with residue = (%d rows, %v), want (1, nil)", len(all), err)
	}
	if err := os.WriteFile(filepath.Join(rows, "v1.json"), []byte(`{"half`), 0o644); err != nil {
		t.Fatalf("stage torn row: %v", err)
	}
	if _, err := s.All(ctx); err == nil {
		t.Error("All over torn row succeeded, want loud refusal")
	} else if !strings.Contains(err.Error(), "v1.json") {
		t.Errorf("refusal names no file: %v", err)
	}
}

// Locks must exclude across store instances (separate processes in
// production — serve, CLI runs): the kernel owns the truth, not the
// struct. If this fails, two processes happily co-hold one lock.
func TestFileStoreLockExcludesAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	a, b := store.NewFileStore(dir), store.NewFileStore(dir)
	ctx := t.Context()
	ok, err := a.AcquireLock(ctx, store.GCLockKey, time.Minute)
	if err != nil || !ok {
		t.Fatalf("A acquire = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := b.AcquireLock(ctx, store.GCLockKey, time.Minute); err != nil || ok {
		t.Errorf("B acquire under A = (%v, %v), want (false, nil)", ok, err)
	}
	if err := a.ReleaseLock(ctx, store.GCLockKey); err != nil {
		t.Fatalf("A release: %v", err)
	}
	if ok, err := b.AcquireLock(ctx, store.GCLockKey, time.Minute); err != nil || !ok {
		t.Errorf("B acquire after release = (%v, %v), want (true, nil)", ok, err)
	}
	_ = b.ReleaseLock(ctx, store.GCLockKey)
}

// Concurrent writers must neither error, deadlock, nor corrupt:
// rows stay parseable and countable after the hammer. Run with
// -race in CI proposal; here plain -count=1 suffices for logic.
func TestFileStoreConcurrentHammer(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	done := make(chan error, 64)
	for w := 0; w < 8; w++ {
		go func(w int) {
			for i := 0; i < 25; i++ {
				tag := "t" + string(rune('a'+w)) + string(rune('0'+i%10))
				if err := s.Record(ctx, policyRow("app", tag)); err != nil {
					done <- err
					return
				}
				if err := s.MarkDue(ctx, "app", tag, "hammer"); err != nil {
					done <- err
					return
				}
				if _, err := s.UnmarkDue(ctx, "app", tag); err != nil {
					done <- err
					return
				}
				if err := s.PushActivity(ctx, store.Outcome{Repo: "app", Tag: tag}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(w)
	}
	for w := 0; w < 8; w++ {
		if err := <-done; err != nil {
			t.Fatalf("hammer worker: %v", err)
		}
	}
	if all, err := s.All(ctx); err != nil {
		t.Fatalf("All after hammer: %v", err)
	} else if len(all) == 0 {
		t.Error("All after hammer empty, want surviving rows")
	}
	if act, err := s.Activity(ctx); err != nil || len(act) == 0 {
		t.Errorf("Activity after hammer = (%d, %v), want entries", len(act), err)
	}
}

func policyRow(repo, tag string) policy.Row {
	return policy.Row{Repo: repo, Tag: tag, Digest: "sha256:abc", PushedAt: time.Now()}
}
