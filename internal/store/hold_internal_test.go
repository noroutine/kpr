package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assertOnly fails unless dir holds exactly the named entries.
func assertOnly(t *testing.T, dir string, what string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != len(want) {
		t.Errorf("dir holds %v %s, want %v", names, what, want)
		return
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("dir holds %v %s, want %v", names, what, want)
			return
		}
	}
}

// The lease lands as exactly one file: no temp residue a crashed
// writer could leave behind, content intact, 0600, gone on
// release. Readers must never see half a lease. If this fails,
// the write path litters the dir or the release leaks.
func TestHoldLandsSingleFile(t *testing.T) {
	dir := t.TempDir()
	h := HoldFile{Dir: dir}
	until := time.Now().Add(time.Minute)
	release, err := h.Hold(context.Background(), until)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	assertOnly(t, dir, "after Hold", HoldFileName)
	entry, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if fi, err := entry[0].Info(); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("lease perm = %o, want 600", fi.Mode().Perm())
	}
	if got, present := h.Read(); !present || !got.Equal(until) {
		t.Errorf("Read = (%v, %v), want (%v, true)", got, present, until)
	}
	release()
	assertOnly(t, dir, "after release")
}

// Write-fail leg: see TestPutFileRefusesDiskFullWrite — the code is
// shared, so its rlimit refusal covers both callers, no seam and
// no second staging here.

// A rename that fails refuses the take: a directory pre-created at
// the lease path makes the swap fail deterministically, and the
// staged temp must be removed. If this fails, failed swaps litter
// temps beside the lease.
func TestHoldRefusesFailedRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, HoldFileName), 0o755); err != nil {
		t.Fatalf("stage blocking dir: %v", err)
	}
	if _, err := (HoldFile{Dir: dir}).Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Error("Hold over a blocked rename succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "write hold lease") {
		t.Errorf("refusal = %q, want the write named", err.Error())
	}
	assertOnly(t, dir, "after refused rename", HoldFileName)
}

// Crash residue never piles up: a stale temp predating the Hold is
// swept as the new lease lands. If this fails, every crashed
// collect leaves a temp file in the fence dir forever.
func TestHoldSweepsStaleTemps(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".edge-fence-123.tmp")
	if err := os.WriteFile(stale, []byte("half a lease"), 0o600); err != nil {
		t.Fatalf("stage stale temp: %v", err)
	}
	release, err := (HoldFile{Dir: dir}).Hold(context.Background(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer release()
	assertOnly(t, dir, "after Hold", HoldFileName)
}

// An empty dir skips the sweep: marker-only gates hold no path,
// so Hold must never touch the working directory. If this fails,
// a dir-less Hold sweeps (and writes its lease into) the cwd.
func TestHoldEmptyDirSkipsSweep(t *testing.T) {
	t.Chdir(t.TempDir())
	stale := ".edge-fence-123.tmp"
	if err := os.WriteFile(stale, []byte("half a lease"), 0o600); err != nil {
		t.Fatalf("stage stale temp: %v", err)
	}
	release, err := (HoldFile{}).Hold(context.Background(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer release()
	assertOnly(t, ".", "after dir-less Hold", stale, HoldFileName)
}

// A lease that cannot land refuses the take: gc must hear the
// failure instead of collecting unfenced. If this fails, a gc
// collects while believing pushes are held.
func TestHoldRefusesBadDir(t *testing.T) {
	h := HoldFile{Dir: filepath.Join(t.TempDir(), "no-such-dir")}
	if _, err := h.Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Error("Hold into a missing dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "write hold lease") {
		t.Errorf("refusal = %q, want the write named", err.Error())
	}
}
