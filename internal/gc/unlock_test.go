package gc

import (
	"context"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
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
		"http://registry:5000", cfg, "/bin/sh", store.NewMemStore(), store.NewMemStore(), store.NewMemStore(), stubClock{}, "time.example.com",
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
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	// Silence with an unpaired store establishes the pairing: the
	// proof carries the fresh identity the store now holds.
	ident, err := s.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if ident.ID == "" {
		t.Error("unlock succeeded but recorded no identity")
	}
	served, _, err := sentinel.Read(context.Background(), fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	if served.ID != ident.ID {
		t.Errorf("unlock proof carries %q, store paired to %q", served.ID, ident.ID)
	}
	if ok, err := s.IsUnlocked(context.Background()); err != nil || !ok {
		t.Fatalf("post-unlock IsUnlocked = (%v, %v), want (true, nil)", ok, err)
	}
	if !strings.Contains(out.String(), "store unlocked: shared store proven via") {
		t.Errorf("unlock names no proof:\n%s", out.String())
	}
	rows, err := s.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(rows) != 1 || rows[0].Tag == sentinel.Tag || rows[0].Actor != "kpr-unlock" {
		t.Errorf("tracked rows = %+v, want the one unlock generation", rows)
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
	if err := Unlock(context.Background(), &out, fileAPI{t.TempDir()}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock on a stranger store succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "does not share") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if ok, err := s.IsUnlocked(context.Background()); err != nil || ok {
		t.Fatalf("post-refusal IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
}

// Unlock reads before it mints: a foreign lineage refuses with the
// adopt ceremony named, the served generation untouched, the marker
// down. If this fails, unlock overwrites a stranger's latest.
func TestUnlockRefusesForeignLineage(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	ctx := context.Background()
	servedGen := stagePairedGen(t, s, root)
	foreign := newGenID(t)
	served, _, err := sentinel.Read(ctx, fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	_ = served
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: servedGen, ID: foreign, TS: served.TS}); err != nil {
		t.Fatalf("stage foreign generation: %v", err)
	}
	var out strings.Builder
	if err := Unlock(ctx, &out, fileAPI{root}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock over a foreign lineage succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "foreign lineage") || !strings.Contains(err.Error(), "kpr store adopt") {
		t.Errorf("refusal names no cause or ceremony: %v", err)
	}
	back, _, err := sentinel.Read(ctx, fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read back served: %v", err)
	}
	if back.ID != foreign || back.Gen != servedGen {
		t.Errorf("served generation overwritten: %+v", back)
	}
	if ok, err := s.IsUnlocked(ctx); err != nil || ok {
		t.Fatalf("post-refusal IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
}

// An unpaired store facing a served lineage refuses too: pairing is
// the operator's explicit ceremony, not a side effect of unlock. If
// this fails, unlock silently pairs to whatever it found.
func TestUnlockRefusesUnpairedWithServed(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	stagePairedGen(t, store.NewMemStore(), root)
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock with an unpaired store succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "unpaired") || !strings.Contains(err.Error(), "kpr store adopt") {
		t.Errorf("refusal names no cause or ceremony: %v", err)
	}
	if ok, err := s.IsUnlocked(context.Background()); err != nil || ok {
		t.Fatalf("post-refusal IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
	ident, _ := s.GetIdentity(context.Background())
	if ident.ID != "" {
		t.Errorf("refused unlock paired to %q", ident.ID)
	}
}

// Unlock judges like gc does: a served rollback refuses instead of
// being minted past — the restore is an incident, not a baseline.
// If this fails, `kpr store unlock` silently buries what `kpr gc` refuses.
func TestUnlockRefusesStaleRollback(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	ctx := context.Background()
	gen := stagePairedGen(t, s, root)
	now := time.Now().UTC()
	if err := s.Record(ctx, newRow(sentinel.Repo, gen, now.Add(-time.Hour))); err != nil {
		t.Fatalf("track served generation: %v", err)
	}
	if err := s.Record(ctx, newRow(sentinel.Repo, newGenID(t), now)); err != nil {
		t.Fatalf("track newer generation: %v", err)
	}
	ident, err := s.GetIdentity(ctx)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if err := s.SetIdentity(ctx, store.Identity{ID: ident.ID}); err != nil {
		t.Fatalf("clear baseline: %v", err)
	}
	var out strings.Builder
	if err := Unlock(ctx, &out, fileAPI{root}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock over a rollback succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "older than tracked") {
		t.Errorf("refusal names no cause: %v", err)
	}
	back, _, err := sentinel.Read(ctx, fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read back served: %v", err)
	}
	if back.Gen != gen {
		t.Errorf("served generation overwritten: got %s want %s", back.Gen, gen)
	}
	if ok, err := s.IsUnlocked(ctx); err != nil || ok {
		t.Fatalf("post-refusal IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
}

// The proof's timestamp comes from a checked clock here too: skew
// refuses (unlock is a manual ceremony — fix NTP and retry, there
// is no --force to hide behind), an unreachable NTP warns through.
func TestUnlockClockSkewRefuses(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	stagePairedGen(t, s, root)
	var out strings.Builder
	err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, s, s, s, stubClock{off: time.Hour}, "time.example.com")
	if err == nil {
		t.Fatal("skewed unlock succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "clock skew") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if ok, err := s.IsUnlocked(context.Background()); err != nil || ok {
		t.Fatalf("post-refusal IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestUnlockNTPUnreachableWarnsProceeds(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	stagePairedGen(t, s, root)
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, s, s, s, stubClock{err: errClockUnreachable}, "time.example.com"); err != nil {
		t.Fatalf("unlock with unreachable NTP: %v", err)
	}
	if !strings.Contains(out.String(), "proceeding with local clock") {
		t.Errorf("unlock warns nothing about the NTP it could not reach:\n%s", out.String())
	}
	// The warning is not the outcome: the proof still lands.
	if ok, err := s.IsUnlocked(context.Background()); err != nil || !ok {
		t.Fatalf("warned unlock IsUnlocked = (%v, %v), want (true, nil)", ok, err)
	}
	served, _, err := sentinel.Read(context.Background(), fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served proof: %v", err)
	}
	ident, err := s.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if served.ID != ident.ID {
		t.Errorf("warned proof carries %q, store paired to %q", served.ID, ident.ID)
	}
}

// Establishing under an already-paired store warns loud: nothing
// served where a lineage exists means the tag was wiped (or the
// mount moved) — re-minting the baseline is the recovery, but the
// operator must hear about it. If this fails, a wiped tag reads as
// a fresh deploy.
func TestUnlockEstablishPairedWarns(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	ctx := context.Background()
	id := newGenID(t)
	adopted := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := s.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: "accepted-gen", AdoptedAt: adopted}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	var out strings.Builder
	if err := Unlock(ctx, &out, fileAPI{root}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err != nil {
		t.Fatalf("re-establish: %v", err)
	}
	if !strings.Contains(out.String(), "nothing served") || !strings.Contains(out.String(), "re-minting") {
		t.Errorf("re-establish warns nothing about the missing tag:\n%s", out.String())
	}
	ident, err := s.GetIdentity(ctx)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if ident.ID != id || ident.BaselineGen != "accepted-gen" || !ident.AdoptedAt.Equal(adopted) {
		t.Errorf("re-establish rewrote the pairing: %+v", ident)
	}
	// The warning is not the outcome: the proof still lands and opens.
	if ok, err := s.IsUnlocked(ctx); err != nil || !ok {
		t.Fatalf("warned unlock IsUnlocked = (%v, %v), want (true, nil)", ok, err)
	}
	served, _, err := sentinel.Read(ctx, fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served baseline: %v", err)
	}
	if served.ID != id {
		t.Errorf("re-minted baseline carries %q, paired to %q", served.ID, id)
	}
}

// A paired store mints the proof under its own identity: the served
// payload after unlock carries the stored ID. If this fails, every
// unlock litters an identity-less generation the next gc refuses.
func TestUnlockMintsStoredIdentity(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	stagePairedGen(t, s, root)
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	ident, err := s.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	served, _, err := sentinel.Read(context.Background(), fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	if served.ID != ident.ID {
		t.Errorf("unlock proof carries %q, store paired to %q", served.ID, ident.ID)
	}
}
