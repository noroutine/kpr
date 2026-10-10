package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// A pipe breaking mid-list surfaces the error: the first removed
// line already printed, so only the per-row check catches it. If
// this fails, a truncated removal list reads as complete.
func TestStoreRmSurfacesRowLineWriteError(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	if err := s.SetUnlocked(cliCtx(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	if err := runStoreRm(cliCtx(), &failAfterWriter{n: 1}, s,
		[]string{"app:v1", "scratch:10m"}, false,
		&sweep.Sweeper{Store: s}, nil, unlockedProof(t, s)); err == nil {
		t.Error("rm failing on the row line succeeded, want an error")
	}
}

func TestStoreRmUntagPartialPrintsDone(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	stub := &flipStub{}
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1", "scratch:10m"}, true,
		&sweep.Sweeper{Store: s, Registry: stub}, untagProof(t, s), unlockedProof(t, s)); err == nil {
		t.Fatal("partial untag succeeded, want the held failure")
	}
	if !strings.Contains(out.String(), "untagged app:v1") {
		t.Errorf("partial untag hid the confirmed row:\n%s", out.String())
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the 1 held row kept", len(rows))
	}
}

// --untag deletes the manifest by digest first and drops the row
// only on confirm — the sweeper's order, without the due mark.
// If this fails, rm either orphans registry tags or drops rows
// for tags that survive.
func TestStoreRmUntagDeletesThenDropsRow(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	stub := &untagStub{outcome: "deleted"}
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, true, &sweep.Sweeper{Store: s, Registry: stub}, untagProof(t, s), unlockedProof(t, s)); err != nil {
		t.Fatalf("runStoreRm untag: %v", err)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "app:sha256:aaa" {
		t.Errorf("untag calls = %v, want [app:sha256:aaa] (by digest, not tag)", stub.calls)
	}
	rows, _ := s.All(cliCtx())
	for _, r := range rows {
		if r.Repo == "app" && r.Tag == "v1" {
			t.Error("untagged row left behind")
		}
	}
	if !strings.Contains(out.String(), "untagged") {
		t.Errorf("rm should say untagged:\n%s", out.String())
	}
}

// The untag path without a token refuses before the first
// manifest: runStoreRm threads the proof through, it never produces
// one. If this fails, the command layer can delete unproven.
func TestStoreRmUntagWithoutProofRefuses(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	// Unlocked ground: this test isolates the missing identity
	// token (locked refusal has its own test below).
	mustUnlock(t, s)
	stub := &untagStub{outcome: "deleted"}
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, true,
		&sweep.Sweeper{Store: s, Registry: stub}, nil, unlockedProof(t, s)); err == nil {
		t.Fatal("proofless rm --untag succeeded, want the blind refusal")
	}
	if len(stub.calls) != 0 {
		t.Errorf("proofless rm --untag attempted %d registry deletes", len(stub.calls))
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("rows = %d, want both kept", len(rows))
	}
}

// Locked refuses before resolution: a locked store forgets nothing
// and deletes nothing. The nil unlocked token IS the locked store
// here — the command produces it via ProveUnlockedStore, which fails
// on a locked marker, so no caller can hold a token for one. If
// this fails, the freeze has a hole at the command layer.
func TestStoreRmLockedRefuses(t *testing.T) {
	for _, untag := range []bool{false, true} {
		s := store.NewMemStore()
		seedRows(s)
		// Paired but locked: identity reads need no marker, so the
		// same-token produces and only intent is missing.
		if err := s.SetIdentity(cliCtx(), store.Identity{ID: "test-id"}); err != nil {
			t.Fatalf("stage identity: %v", err)
		}
		same, err := proof.Prover{
			Sentinel: stubProofAPI{id: "test-id", ts: cliNow.Format(time.RFC3339)},
			Store:    s, Armed: true,
			Now: func() time.Time { return cliNow },
		}.Prove(cliCtx())
		if err != nil {
			t.Fatalf("prove on paired ground: %v", err)
		}
		stub := &untagStub{outcome: "deleted"}
		var out bytes.Buffer
		if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, untag,
			&sweep.Sweeper{Store: s, Registry: stub}, same, nil); err == nil {
			t.Fatalf("untag=%v locked rm succeeded, want the locked refusal", untag)
		} else if !strings.Contains(err.Error(), "locked") {
			t.Errorf("untag=%v err = %q, want the locked refusal named", untag, err.Error())
		}
		if len(stub.calls) != 0 {
			t.Errorf("untag=%v locked rm attempted %d registry deletes", untag, len(stub.calls))
		}
		rows, _ := s.All(cliCtx())
		if len(rows) != 2 {
			t.Errorf("untag=%v rows = %d, want both kept", untag, len(rows))
		}
	}
}

// A held delete (tag fallback on distribution:3, denied configs)
// keeps the row: dropping it would untrack a live tag silently.
func TestStoreRmUntagHeldKeepsRowLoud(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	stub := &untagStub{outcome: "held"}
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, true, &sweep.Sweeper{Store: s, Registry: stub}, untagProof(t, s), unlockedProof(t, s)); err == nil {
		t.Fatal("held untag succeeded, want a loud failure")
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("held untag dropped the row: %d left, want 2", len(rows))
	}
}

// Removing against dead state fails before resolution: nothing
// resolves, nothing drops. If this fails, an outage deletes on
// paper.
func TestStoreRmOnDeadStoreFails(t *testing.T) {
	stub := &untagStub{outcome: "deleted"}
	if err := runStoreRm(cliCtx(), io.Discard, deadStore{}, []string{"app:v1"}, false,
		&sweep.Sweeper{Store: deadStore{}, Registry: stub}, nil, unlockedProof(t, mustUnlockedMem(t))); err == nil {
		t.Error("rm on dead store succeeded, want an error")
	}
}

// An untagged row that cannot print still drops: the registry
// already confirmed, so the failure is loud, not a rollback. If
// this fails, a breaking pipe resurrects deleted rows.
func TestStoreRmUntagRowWriteFailureSurfaces(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	stub := &untagStub{outcome: "deleted"}
	if err := runStoreRm(cliCtx(), &failAfterWriter{}, s, []string{"app:v1"}, true,
		&sweep.Sweeper{Store: s, Registry: stub}, untagProof(t, s), unlockedProof(t, s)); err == nil {
		t.Error("untag rm into breaking pipe succeeded, want an error")
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1 (confirmed drop stands)", len(rows))
	}
}

// rm drops the tracked row only: the registry tag survives,
// untracked until a re-push or backfill re-tracks it. The warning
// is the point — a silent rm would look like a delete.
func TestStoreRmDropsRowWarnsTagStays(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	mustUnlock(t, s)
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, false, &sweep.Sweeper{Store: s}, nil, unlockedProof(t, s)); err != nil {
		t.Fatalf("runStoreRm: %v", err)
	}
	if !strings.Contains(out.String(), "untracked") {
		t.Errorf("rm should warn the tag stays untracked:\n%s", out.String())
	}
	rows, err := s.All(cliCtx())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, r := range rows {
		if r.Repo == "app" && r.Tag == "v1" {
			t.Error("rm left the row behind")
		}
	}
	if len(rows) != 1 {
		t.Errorf("rm took %d rows, want 1 left", len(rows))
	}
}

// rm is all-or-nothing: one unknown ref refuses before anything is
// deleted, so a typo cannot half-clear the store.
func TestStoreRmUnknownRefusesBeforeDeleting(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	// Unknown-ref refusal precedes every gate (resolution first), so
	// the tokens never get read — nils document they are unreachable.
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1", "app:nope"}, false, &sweep.Sweeper{Store: s}, nil, nil); err == nil {
		t.Fatal("rm with an unknown ref should refuse")
	} else if !strings.Contains(err.Error(), "app:nope") {
		t.Errorf("refusal should name the unknown row, got: %v", err)
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("refused rm deleted rows: %d left, want 2", len(rows))
	}
}
