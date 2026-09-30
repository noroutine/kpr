package gc

import (
	"context"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// A locked store refuses before anything else: no probe, no proof,
// no collector — the refusal names the fix. Fresh stores are born
// locked, so this is also the default-deny pin at the use-case
// level (the contract pins it at the backend level). If this fails,
// a fresh deploy collects without ever proving locality.
func TestRunRefusesLockedStore(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out,
		Probe(func(context.Context, string) (Mode, string, error) { return ModeReadonly, "", nil }),
		store.NewMemStore(), okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh",
		Options{DryRun: true, Report: func(Event) {}})
	if err == nil || !strings.Contains(err.Error(), "store is locked") {
		t.Fatalf("locked run = %v, want the locked refusal", err)
	}
}

// Unlock proves the shared store and records intent: the marker
// flips and the output names the proven generation. If this fails,
// a colocated deploy can never open writes.
func TestUnlockProvesAndRecords(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if ok, err := s.IsUnlocked(context.Background()); err != nil || !ok {
		t.Fatalf("post-unlock IsUnlocked = (%v, %v), want (true, nil)", ok, err)
	}
	if !strings.Contains(out.String(), "store unlocked: shared store proven via") {
		t.Errorf("unlock names no proof:\n%s", out.String())
	}
}

// Unlock against a store the API doesn't serve refuses and leaves
// the marker down: intent never opens without proof. The stranger
// root never receives the generation, so the read-back fails. If
// this fails, a remote kpr unlocks against nothing and gc follows.
func TestUnlockRefusesStrangerStore(t *testing.T) {
	cfg, _, _ := stageProvenRun(t)
	s := store.NewMemStore()
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{t.TempDir()}, cfg, s); err == nil {
		t.Fatal("unlock on a stranger store succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "does not share") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if ok, err := s.IsUnlocked(context.Background()); err != nil || ok {
		t.Fatalf("post-refusal IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
}
