package gc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// Missing binary or config refuses with the remedy instead of failing
// mid-collect: gc degrades by construction when the store isn't
// shared into this container. If this fails, a bare image (no mounts)
// crashes on paths instead of explaining them.
func TestReadyGatesMissingPrereqs(t *testing.T) {
	if err := ready("/bin/registry", "/etc/distribution/config.yml"); err != nil {
		t.Logf("note: this host lacks %v (fine outside the image)", err)
	}
	if err := ready("/no/such/binary", "/etc/distribution/config.yml"); err == nil {
		t.Error("missing binary passed readiness, want refusal")
	} else if !strings.Contains(err.Error(), "/no/such/binary") {
		t.Errorf("refusal names no path: %v", err)
	}
	if err := ready("/bin/sh", "/no/such/config.yml"); err == nil {
		t.Error("missing config passed readiness, want refusal")
	} else if !strings.Contains(err.Error(), "/no/such/config.yml") {
		t.Errorf("refusal names no path: %v", err)
	}
}

// An unwritable root refuses the mint before the read-back: the
// generation is never half-laid. If this fails, a read-only mount
// reports a same-store mismatch instead of the write error.
func TestWriteVerifiedRefusesUnwritable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o644); err != nil {
		t.Fatalf("stage blocker: %v", err)
	}
	payload := sentinel.Payload{V: 1, Gen: "gen", ID: "id", TS: "ts", Writer: "test"}
	if _, err := sentinel.WriteVerified(context.Background(), fileAPI{t.TempDir()}, blocker, payload); err == nil {
		t.Fatal("mint under a blocked root succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "unwritable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}
