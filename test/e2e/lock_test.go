//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/storeops"
)

// stageRegistryConfig writes a stock distribution config rooted at
// root: the same shared layout the collector (and unlock) prove
// through proof.FilesystemStore.
func stageRegistryConfig(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := "storage:\n  filesystem:\n    rootdirectory: " + root + "\n"
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("stage registry config: %v", err)
	}
	return path
}

// A fresh state store denies registry-store writes: gc refuses
// before probing, naming `kpr unlock` as the fix. Unlock proves the
// shared store for real (generation onto the bind-mounted root, read
// back through the API) and opens writes; re-lock closes them again
// — locked behaves exactly like no shared store, which is what makes
// the marker the remote-mode simulator. If this fails, a fresh
// deploy collects without proving, or intent doesn't survive a real
// registry round trip.
func TestGCLockGateUnlockOpens(t *testing.T) {
	url, root := startMountedRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	api := registry.NewClient(url)
	cfg := stageRegistryConfig(t, root)
	state := store.NewFileStore(t.TempDir())
	bin, runs := stageCollectorStub(t, "")
	restore := stageRunConfig(t, url, cfg, bin, stageTimeServer(t), "")
	defer restore()
	preview := func() error {
		var out strings.Builder
		return gc.Run(ctx, &out, gc.Deps{
			Lock: state, Rec: state, Ids: state, Rows: state,
			API: api, Clock: clock.HTTPS{}, Report: func(event.Event) {},
			Store: state,
		}, gc.Options{}, gc.Accepts{})
	}

	var out strings.Builder
	err := preview()
	if err == nil || !strings.Contains(err.Error(), "store is locked") {
		t.Fatalf("locked gc = %v, want the locked refusal", err)
	}
	if n := collectorRuns(t, runs); n != 0 {
		t.Fatalf("locked gc reached the collector %d times", n)
	}

	out.Reset()
	if err := storeops.Unlock(ctx, &out, storeops.UnlockDeps{
		Rec: state, Ids: state, Rows: state,
		API: api, Clock: clock.HTTPS{}, Store: state,
	}); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if ok, err := state.IsUnlocked(ctx); err != nil || !ok {
		t.Fatalf("post-unlock IsUnlocked = (%v, %v), want (true, nil)", ok, err)
	}

	out.Reset()
	if err := preview(); err != nil {
		t.Fatalf("unlocked gc: %v", err)
	}
	if n := collectorRuns(t, runs); n != 1 {
		t.Errorf("collector ran %d times, want 1", n)
	}
	rows, rerr := state.All(ctx)
	if rerr != nil {
		t.Fatalf("read rows: %v", rerr)
	}
	if len(rows) != 1 {
		t.Errorf("rows after unlock + dry-run gc = %d, want only the unlock generation", len(rows))
	}

	if err := state.SetUnlocked(ctx, false); err != nil {
		t.Fatalf("re-lock: %v", err)
	}
	out.Reset()
	if err := preview(); err == nil ||
		!strings.Contains(err.Error(), "store is locked") {
		t.Fatalf("re-locked gc = %v, want the locked refusal", err)
	}
}

// Unlock against a registry that doesn't serve the staged store
// refuses and leaves the marker down: the second registry shares
// nothing with the config root, so the read-back fails. If this
// fails, intent opens without proof.
func TestUnlockRefusesUnsharedRegistry(t *testing.T) {
	url, _ := startMountedRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	api := registry.NewClient(url)
	cfg := stageRegistryConfig(t, t.TempDir())
	state := store.NewFileStore(t.TempDir())

	restore := stageRunConfig(t, url, cfg, "", stageTimeServer(t), "")
	defer restore()
	var out strings.Builder
	if err := storeops.Unlock(ctx, &out, storeops.UnlockDeps{
		Rec: state, Ids: state, Rows: state,
		API: api, Clock: clock.HTTPS{}, Store: state,
	}); err == nil || !strings.Contains(err.Error(), "does not share") {
		t.Fatalf("stranger unlock = %v, want the no-shared-store refusal", err)
	}
	if ok, err := state.IsUnlocked(ctx); err != nil || ok {
		t.Fatalf("post-refusal IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
}
