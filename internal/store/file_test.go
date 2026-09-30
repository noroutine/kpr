package store_test

import (
	"encoding/json"
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

// A MkdirAll failure serializing mutations refuses: without the dir
// lock every later op races. Pointing the store under a file makes
// the mkdir fail deterministically. If this fails, a read-only or
// broken mount writes half-serialized state.
func TestFileStoreMkdirFailureRefuses(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("stage blocker file: %v", err)
	}
	s := store.NewFileStore(filepath.Join(blocker, "sub"))
	if err := s.Record(t.Context(), policyRow("app", "v1")); err == nil {
		t.Error("Record under an unmakable dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("refusal = %v, want the mkdir failure (not fallthrough)", err)
	}
}

// A rename failure is a write failure, never a silent success: the
// temp holds the only copy of the new bytes. A non-empty directory
// sitting at the singleton path fails the rename deterministically —
// via SetCurrent, which has no pre-read to fail first (row writes
// pre-read, so they refuse earlier for other reasons). If this
// fails, full disks report success and lose state.
func TestFileStoreRenameFailureRefuses(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	blocker := filepath.Join(dir, "current.json")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatalf("stage dir at singleton path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatalf("fill blocker dir: %v", err)
	}
	if err := s.SetCurrent(ctx, store.Current{PassID: "p1"}); err == nil {
		t.Error("SetCurrent over an unrenamable path succeeded, want refusal")
	}
}

// Ping proves writability, not just existence: the mute mutant
// (MkdirAll error check negated) returns early on success, skipping
// the probe file entirely. A read-only dir must refuse (skipped for
// root, which ignores permission bits). If this fails, Ping greens a
// store nothing can write to.
func TestFileStorePingProvesWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, nothing is unwritable")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if err := store.NewFileStore(ro).Ping(t.Context()); err == nil {
		t.Error("Ping on a read-only dir succeeded, want refusal")
	}
}

// Ping cleans its probe: the mute mutant (CreateTemp error check
// negated) returns early on success, abandoning a .ping-* file per
// call. A fresh dir pings to zero entries. If this fails, every Ping
// litters the store root.
func TestFileStorePingLeavesNoResidue(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("Ping left %v, want a clean dir", names)
	}
}

// Deleting a row that cannot be removed (a non-empty directory in
// its place) errors: only absence is a no-op. If this fails, rows
// survive Delete while the caller believes them gone.
func TestFileStoreDeleteFailureRefuses(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if err := s.Record(ctx, policyRow("app", "v1")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rowPath := filepath.Join(dir, "rows", "app", "v1.json")
	if err := os.Remove(rowPath); err != nil {
		t.Fatalf("unstage row: %v", err)
	}
	if err := os.MkdirAll(rowPath, 0o755); err != nil {
		t.Fatalf("stage dir at row path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rowPath, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatalf("fill row dir: %v", err)
	}
	if err := s.Delete(ctx, "app", "v1"); err == nil {
		t.Error("Delete of an unremovable row succeeded, want refusal")
	}
}

// Torn singletons refuse like torn rows: current.json and
// activity.json are ours to write well-formed, so garbage in them
// is foreign or disk trouble — loud, never zero-valued. If this
// fails, a corrupt activity ring reads as an empty one.
func TestFileStoreTornSingletonsRefuse(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if err := os.WriteFile(filepath.Join(dir, "activity.json"), []byte(`{"half`), 0o644); err != nil {
		t.Fatalf("stage torn activity: %v", err)
	}
	if _, err := s.Activity(ctx); err == nil {
		t.Error("Activity over torn file succeeded, want loud refusal")
	}
	if err := os.WriteFile(filepath.Join(dir, "current.json"), []byte(`{"half`), 0o644); err != nil {
		t.Fatalf("stage torn current: %v", err)
	}
	if _, err := s.GetCurrent(ctx); err == nil {
		t.Error("GetCurrent over torn file succeeded, want loud refusal")
	}
}

// The lock claim names the holder host: its whole purpose is the
// operator's cat. The mute mutant (hostname error check negated)
// claims "unknown" on success. If this fails, every lock reads as
// held by nobody.
func TestFileStoreLockClaimNamesHost(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if ok, err := s.AcquireLock(ctx, store.GCLockKey, time.Minute); err != nil || !ok {
		t.Fatalf("acquire = (%v, %v), want (true, nil)", ok, err)
	}
	defer func() { _ = s.ReleaseLock(ctx, store.GCLockKey) }()
	raw, err := os.ReadFile(filepath.Join(dir, "locks", store.GCLockKey+".lock"))
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	host, herr := os.Hostname()
	if herr != nil {
		t.Skipf("no hostname here: %v", herr)
	}
	var claim struct {
		Holder string `json:"holder"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		t.Fatalf("claim unparseable: %v", err)
	}
	if claim.Holder != host {
		t.Errorf("claim holder = %q, want hostname %q", claim.Holder, host)
	}
}

// Close releases held locks: a new instance acquires right after.
// If this fails, shutdown leaks locks into kernel cleanup instead of
// handing them over.
func TestFileStoreCloseReleasesLocks(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if ok, err := s.AcquireLock(ctx, store.GCLockKey, time.Minute); err != nil || !ok {
		t.Fatalf("acquire = (%v, %v), want (true, nil)", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	other := store.NewFileStore(dir)
	if ok, err := other.AcquireLock(ctx, store.GCLockKey, time.Minute); err != nil || !ok {
		t.Errorf("acquire after Close = (%v, %v), want (true, nil)", ok, err)
	}
	_ = other.Close()
}
