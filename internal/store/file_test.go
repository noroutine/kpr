package store_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/testing/storetest"
)

// The file store must satisfy the same contract as mem and redis:
// one suite, never a copy per backend. If this fails, the redis-less
// mode reasons differently about rows, marks, or locks.
// A lock-removal failure is a refusal, never a quiet unlock: a
// non-empty directory sitting at the marker path fails the remove
// deterministically on any user (no permission tricks, root-proof).
// If this fails, a stuck marker reads cleared while still on disk.
func TestFileStoreLockRemovalFailureRefuses(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	if err := s.SetUnlocked(t.Context(), true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	marker := filepath.Join(dir, "unlocked")
	if err := os.Remove(marker); err != nil {
		t.Fatalf("stage marker removal: %v", err)
	}
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatalf("stage marker dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(marker, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("stage marker child: %v", err)
	}
	if err := s.SetUnlocked(t.Context(), false); err == nil {
		t.Error("lock over an unremovable marker succeeded, want refusal")
	}
}

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

// blockedStore roots a store under a file: no dir in it can ever
// come into being, so every mutating op refuses at its own gate.
func blockedStore(t *testing.T) *store.FileStore {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("stage blocker: %v", err)
	}
	return store.NewFileStore(filepath.Join(blocker, "sub"))
}

// A fresh store scans empty, never missing: the rows tree
// doesn't exist until the first record, and a vanished entry
// mid-walk skips instead of refusing. If this fails, empty
// stores error where they should list nothing.
func TestFileStoreFreshStoreScansEmpty(t *testing.T) {
	s := store.NewFileStore(t.TempDir())
	ctx := t.Context()
	if rows, err := s.All(ctx); err != nil || len(rows) != 0 {
		t.Errorf("All fresh = (%v, %v), want (empty, nil)", rows, err)
	}
	if rows, err := s.Due(ctx); err != nil || len(rows) != 0 {
		t.Errorf("Due fresh = (%v, %v), want (empty, nil)", rows, err)
	}
}

// A held lock reads false on re-acquire, even in-process: the
// open handle keeps the flock, so single-flight holds within one
// holder too. If this fails, the same process takes its own lock
// twice. (The old-handle replacement below the Flock is
// defensive: an entry always means self-locked, so the Flock
// above never passes with one present.)
func TestFileStoreHeldLockReadsFalse(t *testing.T) {
	s := store.NewFileStore(t.TempDir())
	ctx := t.Context()
	if ok, err := s.AcquireLock(ctx, "k", time.Minute); err != nil || !ok {
		t.Fatalf("first AcquireLock = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := s.AcquireLock(ctx, "k", time.Minute); err != nil || ok {
		t.Errorf("second AcquireLock = (%v, %v), want (false, nil)", ok, err)
	}
}

// A torn activity ring fails the push that meets it: the ring
// appends to what it reads, and garbage reads as failure, never
// as an empty ring to overwrite. If this fails, a push buries
// the torn history silently.
func TestFileStorePushActivityOverTornRingRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "activity.json"), []byte(`{"half`), 0o644); err != nil {
		t.Fatalf("stage torn activity: %v", err)
	}
	if err := store.NewFileStore(dir).PushActivity(t.Context(), store.Outcome{Repo: "app"}); err == nil {
		t.Error("PushActivity over torn ring succeeded, want refusal")
	}
}

// Mutations against an unmakable dir refuse at their own gate:
// record, mark, unmark, delete, clear, current, activity, lock,
// identity, ping — no op invents state it cannot hold. If this
// fails, one op succeeds on paper while the disk refused.
func TestFileStoreBlockedDirRefusesEveryOp(t *testing.T) {
	s := blockedStore(t)
	ctx := t.Context()
	row := policyRow("app", "v1")
	if err := s.Record(ctx, row); err == nil {
		t.Error("Record on blocked dir succeeded, want refusal")
	}
	if err := s.MarkDue(ctx, "app", "v1", "x"); err == nil {
		t.Error("MarkDue on blocked dir succeeded, want refusal")
	}
	if _, err := s.UnmarkDue(ctx, "app", "v1"); err == nil {
		t.Error("UnmarkDue on blocked dir succeeded, want refusal")
	}
	if err := s.Delete(ctx, "app", "v1"); err == nil {
		t.Error("Delete on blocked dir succeeded, want refusal")
	}
	if _, err := s.ClearDue(ctx); err == nil {
		t.Error("ClearDue on blocked dir succeeded, want refusal")
	}
	if err := s.SetCurrent(ctx, store.Current{PassID: "p1"}); err == nil {
		t.Error("SetCurrent on blocked dir succeeded, want refusal")
	}
	if err := s.PushActivity(ctx, store.Outcome{Repo: "app"}); err == nil {
		t.Error("PushActivity on blocked dir succeeded, want refusal")
	}
	if _, err := s.AcquireLock(ctx, "x", time.Minute); err == nil {
		t.Error("AcquireLock on blocked dir succeeded, want refusal")
	}
	if err := s.SetIdentity(ctx, store.Identity{ID: "id"}); err == nil {
		t.Error("SetIdentity on blocked dir succeeded, want refusal")
	}
	if err := s.Ping(ctx); err == nil {
		t.Error("Ping on blocked dir succeeded, want refusal")
	}
}

// Bad names refuse before any I/O: empty tags and traversals never
// reach the filesystem. If this fails, crafted names escape the
// rows tree.
func TestFileStoreBadNamesRefuseBeforeIO(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if err := s.Record(ctx, policyRow("a/../b", "v1")); err == nil {
		t.Error("Record with traversal repo succeeded, want refusal")
	}
	// Traversal tags neutralize, never escape: they record under rows
	// (escaped) while the unescaped targets stay absent. If this
	// fails, crafted tags write outside the rows tree.
	for _, tag := range []string{"../evil", "a/b"} {
		if err := s.Record(ctx, policyRow("app", tag)); err != nil {
			t.Errorf("Record with %q tag refused: %v", tag, err)
		}
	}
	for _, outside := range []string{"evil.json", filepath.Join("app", "a")} {
		if _, err := os.Lstat(filepath.Join(dir, "rows", outside)); !os.IsNotExist(err) {
			t.Errorf("unescaped %q exists, want containment under rows", outside)
		}
	}
	if err := s.MarkDue(ctx, "app", "", "x"); err == nil {
		t.Error("MarkDue with empty tag succeeded, want refusal")
	}
	if _, err := s.UnmarkDue(ctx, "app", ""); err == nil {
		t.Error("UnmarkDue with empty tag succeeded, want refusal")
	}
	if err := s.Delete(ctx, "..", "v1"); err == nil {
		t.Error("Delete with traversal repo succeeded, want refusal")
	}
}

// Corrupt rows refuse the op that meets them: record, mark, and
// unmark read before writing, so garbage fails them all. If this
// fails, one op overwrites corruption silently.
func TestFileStoreTornRowRefusesReaders(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	rowDir := filepath.Join(dir, "rows", "app")
	if err := os.MkdirAll(rowDir, 0o755); err != nil {
		t.Fatalf("stage row dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rowDir, "v1.json"), []byte(`{"half`), 0o644); err != nil {
		t.Fatalf("stage torn row: %v", err)
	}
	if err := s.Record(ctx, policyRow("app", "v1")); err == nil {
		t.Error("Record over torn row succeeded, want refusal")
	}
	if err := s.MarkDue(ctx, "app", "v1", "x"); err == nil {
		t.Error("MarkDue over torn row succeeded, want refusal")
	}
	if _, err := s.UnmarkDue(ctx, "app", "v1"); err == nil {
		t.Error("UnmarkDue over torn row succeeded, want refusal")
	}
	if _, err := s.Due(ctx); err == nil {
		t.Error("Due over torn row succeeded, want refusal")
	}
	if _, err := s.ClearDue(ctx); err == nil {
		t.Error("ClearDue over torn row succeeded, want refusal")
	}
}

// Reading a directory as a row fails with the OS cause: mark meets
// it on the pre-read. If this fails, layout trouble reads as
// corruption.
func TestFileStoreDirAsRowRefuses(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if err := os.MkdirAll(filepath.Join(dir, "rows", "app", "v1.json"), 0o755); err != nil {
		t.Fatalf("stage dir at row path: %v", err)
	}
	if err := s.MarkDue(ctx, "app", "v1", "x"); err == nil {
		t.Error("MarkDue over a directory succeeded, want refusal")
	}
}

// A missing singleton reads as empty, never as an error — but a
// directory (or garbage) in its place refuses. Fresh stores are
// empty by construction. If this fails, first boots error (or
// corrupt singletons read zero-valued).
func TestFileStoreSingletonsMissingAndForeign(t *testing.T) {
	s := store.NewFileStore(t.TempDir())
	ctx := t.Context()
	if cur, err := s.GetCurrent(ctx); err != nil || cur != (store.Current{}) {
		t.Errorf("fresh GetCurrent = (%v, %v), want (empty, nil)", cur, err)
	}
	asDir := func(name string) *store.FileStore {
		d := t.TempDir()
		if err := os.MkdirAll(filepath.Join(d, name), 0o755); err != nil {
			t.Fatalf("stage dir singleton: %v", err)
		}
		return store.NewFileStore(d)
	}
	if _, err := asDir("current.json").GetCurrent(ctx); err == nil {
		t.Error("GetCurrent over a directory succeeded, want refusal")
	}
	if _, err := asDir("activity.json").Activity(ctx); err == nil {
		t.Error("Activity over a directory succeeded, want refusal")
	}
	if _, err := asDir("identity.json").GetIdentity(ctx); err == nil {
		t.Error("GetIdentity over a directory succeeded, want refusal")
	}
	garbageDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(garbageDir, "identity.json"), []byte(`{"half`), 0o644); err != nil {
		t.Fatalf("stage torn identity: %v", err)
	}
	if _, err := store.NewFileStore(garbageDir).GetIdentity(ctx); err == nil {
		t.Error("GetIdentity over torn file succeeded, want refusal")
	}
}

// An unreadable rows tree refuses the scan: collect, due, and
// clear all fail naming the outage, never an empty world. Skipped
// for root. If this fails, an unreadable store scans as empty.
func TestFileStoreUnreadableRowsRefuseScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, nothing is unreadable")
	}
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if err := s.Record(ctx, policyRow("app", "v1")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rows := filepath.Join(dir, "rows")
	if err := os.Chmod(rows, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(rows, 0o755) })
	if _, err := s.All(ctx); err == nil {
		t.Error("All over unreadable rows succeeded, want refusal")
	}
	if _, err := s.Due(ctx); err == nil {
		t.Error("Due over unreadable rows succeeded, want refusal")
	}
	if _, err := s.ClearDue(ctx); err == nil {
		t.Error("ClearDue over unreadable rows succeeded, want refusal")
	}
}

// The store root voices itself for operators: the one backend
// detail the dashboard renders. If this fails, the console names
// the wrong root.
func TestFileStoreDirVoicesRoot(t *testing.T) {
	dir := t.TempDir()
	if got := store.NewFileStore(dir).Dir(); got != dir {
		t.Errorf("Dir() = %q, want %q", got, dir)
	}
}

// A stat failure other than not-exist refuses, never guesses: the
// marker under an unreadable dir is unknown, not locked. Skipped
// for root, which ignores permission bits. If this fails, an
// unreadable marker reads as intent.
func TestFileStoreUnreadableMarkerRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, nothing is unreadable")
	}
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	if err := s.SetUnlocked(t.Context(), true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := s.IsUnlocked(t.Context()); err == nil {
		t.Error("IsUnlocked under an unreadable dir succeeded, want refusal")
	}
}

// A full disk refuses the lock claim write: the take fails loud
// with no claim recorded, and no temp stays behind. If this fails,
// a full disk takes locks it cannot evidence.
//
// The limit lives in a child process: RLIMIT_FSIZE is process-global
// and the harness appends its own test log throughout the run, so
// narrowing it in-process broke the harness itself (a framework
// write inside the window failed the package with no failed test).
// The child sets a zero ceiling, attempts the claim, and exits 0
// only on refusal; the parent never narrows its own limits.
func TestFileStoreAcquireLockOnFullDiskFails(t *testing.T) {
	if dir := os.Getenv("KPR_TEST_FULLDISK_DIR"); dir != "" {
		var old syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			os.Exit(2)
		}
		cur := old
		cur.Cur = 0
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &cur); err != nil {
			os.Exit(2)
		}
		s := store.NewFileStore(dir)
		if _, err := s.AcquireLock(t.Context(), "k", time.Minute); err == nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestFileStoreAcquireLockOnFullDiskFails")
	cmd.Env = append(os.Environ(), "KPR_TEST_FULLDISK_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("full-disk child = %v, output:\n%s", err, out)
	}
}

// Re-acquiring a held lock fails closed without touching the held
// file: flock entries are per-open-description, so even the holder
// cannot take its own lock twice. If this fails, one process
// double-holds and the handover logic rots.
func TestFileStoreReacquireHeldFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if ok, err := s.AcquireLock(ctx, "k", time.Minute); err != nil || !ok {
		t.Fatalf("first acquire = (%v, %v), want (true, nil)", ok, err)
	}
	defer func() { _ = s.ReleaseLock(ctx, "k") }()
	if ok, err := s.AcquireLock(ctx, "k", time.Minute); err != nil || ok {
		t.Errorf("re-acquire held = (%v, %v), want (false, nil)", ok, err)
	}
}

// An unwritable rows tree refuses the clear that must rewrite it:
// collect reads fine, the rewrite fails. Skipped for root. If this
// fails, a read-only store clears on paper.
func TestFileStoreClearDueOnReadonlyRowsFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, nothing is unwritable")
	}
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	ctx := t.Context()
	if err := s.Record(ctx, policyRow("app", "v1")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.MarkDue(ctx, "app", "v1", "x"); err != nil {
		t.Fatalf("MarkDue: %v", err)
	}
	leaf := filepath.Join(dir, "rows", "app")
	if err := os.Chmod(leaf, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(leaf, 0o755) })
	if _, err := s.ClearDue(ctx); err == nil {
		t.Error("ClearDue on readonly rows succeeded, want refusal")
	}
}

// An unwritable rows tree refuses the lock take: the lock file
// cannot be created. Skipped for root. If this fails, lock takes
// succeed on paper where no file can land.
func TestFileStoreAcquireLockOnReadonlyLocksFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, nothing is unwritable")
	}
	dir := t.TempDir()
	locks := filepath.Join(dir, "locks")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("stage locks dir: %v", err)
	}
	if err := os.Chmod(locks, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locks, 0o755) })
	s := store.NewFileStore(dir)
	if _, err := s.AcquireLock(t.Context(), "k", time.Minute); err == nil {
		t.Error("AcquireLock into readonly locks succeeded, want refusal")
	}
}

// A due row whose name no longer maps to a file refuses the sweep:
// ClearDue names the un-mappable row instead of skipping it (a
// hand-planted or hostile name must stay loud, never silently kept
// due). If this fails, corrupt rows dirs clear around the rot.
func TestFileStoreClearDueOnUnmappableRowRefuses(t *testing.T) {
	dir := t.TempDir()
	rows := filepath.Join(dir, "rows")
	if err := os.MkdirAll(rows, 0o755); err != nil {
		t.Fatalf("stage rows dir: %v", err)
	}
	raw, err := json.Marshal(policy.Row{Repo: "..", Tag: "x", Due: true})
	if err != nil {
		t.Fatalf("stage row: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rows, "evil.json"), raw, 0o644); err != nil {
		t.Fatalf("plant row: %v", err)
	}
	s := store.NewFileStore(dir)
	if _, err := s.ClearDue(t.Context()); err == nil {
		t.Error("ClearDue over un-mappable row succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "bad path element") {
		t.Errorf("refusal = %q, want it to name the bad path element", err)
	}
}
