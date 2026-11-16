//go:build unix

package resources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stat that fails for something other than absence refuses
// loudly: no permission is not the same as no folder. Skipped
// for root (permissions don't bind it) and windows.
func TestLoadRefusesUnreadableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through permissions; refusal unstagiable")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	if _, err := Load(filepath.Join(sub, "x.yaml")); err == nil {
		t.Error("Load under an unreadable dir succeeded, want the stat refusal")
	}
}

// An unreadable walk entry refuses the whole load with the
// entry named: a half-walked folder is half config. Same
// platform guards as the unreadable-dir case.
func TestLoadRefusesUnreadableEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through permissions; refusal unstagiable")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(locked, "r.yaml"), []byte("kind: TLSBundle\n"))
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(locked, 0o755) }()
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "walk") {
		t.Errorf("unreadable entry Load err = %v, want a walk refusal", err)
	}
}

// An unreadable file refuses with its own name: the folder
// listed it, but its bytes stay shut. Same platform guards
// as the unreadable-dir case.
func TestLoadRefusesUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through permissions; refusal unstagiable")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "shut.yaml")
	writeFile(t, file, []byte("kind: TLSBundle\n"))
	if err := os.Chmod(file, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(file, 0o600) }()
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "read") {
		t.Errorf("unreadable file Load err = %v, want a read refusal", err)
	}
}
