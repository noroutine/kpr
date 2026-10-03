package proof

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStoreConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// A filesystem root mints carrying the root it names, so a walk
// reads the proven path, never a second parse. If this fails,
// local walks stopped proving local.
func TestProveFilesystemStoreMints(t *testing.T) {
	path := writeStoreConfig(t, "storage:\n  filesystem:\n    rootdirectory: /var/lib/registry\n")
	fs, err := ProveFilesystemStore(path)
	if err != nil {
		t.Fatalf("filesystem config = %v, want mint", err)
	}
	if fs == nil {
		t.Fatal("minted nil FilesystemStore, want token")
	}
	if fs.Root() != "/var/lib/registry" {
		t.Errorf("Root() = %q, want the configured root", fs.Root())
	}
}

// s3 and friends refuse: a local walk over object storage is
// bytes never read. If this fails, non-local stores prove local.
func TestProveFilesystemStoreRefusesS3(t *testing.T) {
	path := writeStoreConfig(t, "storage:\n  s3:\n    bucket: kpr\n")
	if _, err := ProveFilesystemStore(path); err == nil {
		t.Error("s3 config minted, want refusal")
	}
}

// A filesystem key with no root refuses: the key existing is not
// the root existing. If this fails, half-configured storage
// proves local.
func TestProveFilesystemStoreRefusesEmptyFilesystem(t *testing.T) {
	path := writeStoreConfig(t, "storage:\n  filesystem: {}\n")
	if _, err := ProveFilesystemStore(path); err == nil {
		t.Error("rootless filesystem config minted, want refusal")
	}
}

// Garbage refuses with the parse named. If this fails, broken
// configs prove local.
func TestProveFilesystemStoreRefusesGarbage(t *testing.T) {
	path := writeStoreConfig(t, "storage: [unclosed\n")
	if _, err := ProveFilesystemStore(path); err == nil {
		t.Error("garbage config minted, want refusal")
	}
}

// A missing file errors (not mints): the refusal names the
// absence. If this fails, an absent config proves local.
func TestProveFilesystemStoreMissingFile(t *testing.T) {
	if _, err := ProveFilesystemStore(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Error("absent config minted, want refusal")
	}
}
