package store

// Internal sabotage: the layout helpers refuse bad inputs, and the
// atomic writer refuses a broken filesystem at every stage. If any
// of these fail, corrupt names write outside the root or a full
// disk mints silently.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Element escaping rejects empties, dots, and traversals: one bad
// element must never become a path. If this fails, a crafted repo
// writes outside the store root.
func TestEscRejectsBadElements(t *testing.T) {
	for _, bad := range []string{"", ".", ".."} {
		if _, err := esc(bad); err == nil {
			t.Errorf("esc(%q) accepted, want refusal", bad)
		}
	}
	if got, err := esc("noroutine"); err != nil || got == "" {
		t.Errorf("esc(noroutine) = (%q, %v), want the escaped name", got, err)
	}
}

// Row files need a tag and clean elements: empty tags and
// traversals refuse before any read. If this fails, reads resolve
// outside the rows tree.
func TestRowFileRefusesBadNames(t *testing.T) {
	s := NewFileStore(t.TempDir())
	if _, err := s.rowFile("app", ""); err == nil {
		t.Error("rowFile with empty tag accepted, want refusal")
	}
	if _, err := s.rowFile("a/../b", "v1"); err == nil {
		t.Error("rowFile with traversal accepted, want refusal")
	}
	if _, err := s.rowFile("app", ".."); err == nil {
		t.Error("rowFile with dot-dot tag accepted, want refusal")
	}
}

// blockFile plants a file where a directory must go: every MkdirAll
// below it fails.
func blockFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("stage parent: %v", err)
	}
	if err := os.WriteFile(path, []byte("in the way"), 0o644); err != nil {
		t.Fatalf("stage block: %v", err)
	}
}

// A blocked filesystem refuses at every putFile stage: MkdirAll,
// temp creation, and the final switch. If this fails, a full or
// broken disk writes partial state as success.
func TestPutFileRefusesBlockedStages(t *testing.T) {
	t.Run("mkdir", func(t *testing.T) {
		root := t.TempDir()
		blockFile(t, filepath.Join(root, "sub"))
		if err := putFile(filepath.Join(root, "sub", "leaf", "f"), []byte("x")); err == nil {
			t.Error("putFile under a blocked dir succeeded, want refusal")
		}
	})
	t.Run("create", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores permission bits, nothing is unwritable")
		}
		root := t.TempDir()
		dir := filepath.Join(root, "ro")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("stage readonly: %v", err)
		}
		defer func() { _ = os.Chmod(dir, 0o755) }()
		if err := putFile(filepath.Join(dir, "f"), []byte("x")); err == nil {
			t.Error("putFile into a readonly dir succeeded, want refusal")
		}
	})
	t.Run("rename", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "f")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatalf("stage target dir: %v", err)
		}
		if err := putFile(target, []byte("x")); err == nil {
			t.Error("putFile over a directory succeeded, want refusal")
		}
	})
}

// A file-too-large disk refuses the write with cleanup: the temp
// never stays behind, and the error names the failure. The rlimit
// window is tiny and restored by defer — framework writes during
// it stay well under the 512-byte ceiling.
func TestPutFileRefusesDiskFullWrite(t *testing.T) {
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatalf("get rlimit: %v", err)
	}
	cur := old
	cur.Cur = 512
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &cur); err != nil {
		t.Fatalf("set rlimit: %v", err)
	}
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) }()

	root := t.TempDir()
	big := make([]byte, 65536)
	for i := range big {
		big[i] = 'x'
	}
	if err := putFile(filepath.Join(root, "f"), big); err == nil {
		t.Error("putFile over the file-size limit succeeded, want refusal")
	}
	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("disk-full write left %d temp files, want none", len(left))
	}
}

// Reading a directory as a row fails with the OS cause, not a
// parse error for bytes never read. If this fails, permission or
// layout trouble reads as corruption.
func TestReadRowRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := readRow(dir); err == nil {
		t.Error("readRow on a directory succeeded, want refusal")
	}
}

// The dir lock refuses when the lock file cannot open: mutual
// exclusion that cannot exclude must fail loud, never pretend.
// If this fails, two writers believe each holds the lock.
func TestDirLockRefusesBlockedLockFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, nothing is unwritable")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("stage readonly: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	s := NewFileStore(dir)
	if _, err := s.dirLock(); err == nil {
		t.Error("dirLock in a readonly dir succeeded, want refusal")
	}
}
