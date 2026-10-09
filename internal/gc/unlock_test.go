package gc

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/event"
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
	stageConfig(t, "http://registry:5000", cfg)
	err := Run(context.Background(), &out,
		Deps{Lock: store.NewMemStore(), Rec: store.NewMemStore(), Ids: store.NewMemStore(), Rows: store.NewMemStore(), API: fileAPI{root},
			Clock: stubClock{}, Report: func(event.Event) {},
			Probe:   Probe(func(context.Context, string) (Mode, string, error) { return ModeReadonly, "", nil }),
			Collect: okCollector(&collected)},
		Options{}, Accepts{})
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

// Unlock pushes the generation under its own tag as well as the
// floater: the recorded row names a tag that exists. If this
// fails, tracked generations dangle from birth.
func TestUnlockPushesGenerationTag(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	ctx := context.Background()
	var out strings.Builder
	if err := Unlock(ctx, &out, fileAPI{root}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	rows, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("tracked rows = %d, want the one unlock generation", len(rows))
	}
	gen := rows[0].Tag
	got, _, err := sentinel.Read(ctx, fileAPI{root}, sentinel.Repo, gen)
	if err != nil {
		t.Fatalf("read generation tag %s: %v (row names a tag nothing pushed)", gen, err)
	}
	if got.Gen != gen {
		t.Errorf("generation tag carries %q, want %q", got.Gen, gen)
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
// is no accept flag to hide behind), an unreachable NTP warns through.
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

// errUnlockStore fails the intent marker: the backend-outage
// stand-in for the lock flag.
type errUnlockStore struct{ err error }

func (e errUnlockStore) SetUnlocked(context.Context, bool) error { return e.err }

// A config with no filesystem root refuses before the clock check:
// unlock proves a local store or nothing. If this fails, unlock
// mints against a layout it cannot prove local.
func TestUnlockS3ConfigRefuses(t *testing.T) {
	_, root, _ := stageProvenRun(t)
	s3cfg := t.TempDir() + "/config.yml"
	if err := os.WriteFile(s3cfg, []byte("storage:\n  s3:\n    bucket: blobs\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	s := store.NewMemStore()
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, s3cfg, s, s, s, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock with s3 config succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "no filesystem storage root") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// An NTP warning that cannot print fails unlock: the ceremony is
// manual, and its warnings are mandatory. If this fails, an
// air-gapped unlock proceeds with no trace of the missed clock.
func TestUnlockUnreachableWarnWriteFailureSurfaces(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	stagePairedGen(t, s, root)
	w := errWriter{errTestStoreDown}
	if err := Unlock(context.Background(), w, fileAPI{root}, cfg, s, s, s, s, stubClock{err: errClockUnreachable}, "time.example.com"); err == nil {
		t.Fatal("unlock with dead output succeeded, want failure")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("failure names no output cause: %v", err)
	}
}

// Untracked state refuses before the verdict, same as gc: silence
// means stranger, empty, or down. If this fails, a backend outage
// at the rows reads as a clean store.
func TestUnlockTrackedStateFailureRefuses(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	rows := failRows{MemStore: s, allErr: errTestStoreDown}
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, s, s, rows, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock with unreadable rows succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "tracked state unreadable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// An unreadable lineage refuses before the verdict for the same
// reason. If this fails, a backend outage at the lineage unlocks
// against an unknown pairing.
func TestUnlockLineageFailureRefuses(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	ids := errIdentityStore{errTestStoreDown}
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, s, ids, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock with unreadable lineage succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unreadable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A re-mint warning that cannot print fails unlock: a wiped tag is
// an incident, and the incident must print. If this fails, the
// recovery proceeds with no trace of the wipe.
func TestUnlockEstablishWarnWriteFailureSurfaces(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	ctx := context.Background()
	if err := s.SetIdentity(ctx, store.Identity{ID: newGenID(t)}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	w := errWriter{errTestStoreDown}
	if err := Unlock(ctx, w, fileAPI{root}, cfg, s, s, s, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("re-establish with dead output succeeded, want failure")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("failure names no output cause: %v", err)
	}
}

// An unrecordable pairing refuses after the proof window: same
// orphan rule as gc. If this fails, a backend outage mid-ceremony
// reads as paired.
func TestUnlockLineageWriteFailureRefuses(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	ids := &failIdentityStore{MemStore: store.NewMemStore(), armed: true}
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, s, ids, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock with unrecordable lineage succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unrecordable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A generation the keep-N log cannot take fails unlock loudly: the
// proof is held but untracked. If this fails, a backend outage at
// the log litters silently.
func TestUnlockMintRecordFailureRefuses(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	rec := errRecorder{errTestStoreDown}
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, s, rec, s, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock with failing keep-N log succeeded, want failure")
	} else if !strings.Contains(err.Error(), "generation went untracked") {
		t.Errorf("failure names no cause: %v", err)
	}
}

// An intent marker that cannot flip fails unlock loudly: the proof
// is held and tracked, but writes stay closed. If this fails, a
// backend outage at the marker reads as unlocked.
func TestUnlockMarkerFailureRefuses(t *testing.T) {
	cfg, root, _ := stageProvenRun(t)
	s := store.NewMemStore()
	st := errUnlockStore{errTestStoreDown}
	var out strings.Builder
	if err := Unlock(context.Background(), &out, fileAPI{root}, cfg, st, s, s, s, stubClock{}, "time.example.com"); err == nil {
		t.Fatal("unlock with failing marker succeeded, want failure")
	} else if !strings.Contains(err.Error(), "intent marker failed") {
		t.Errorf("failure names no cause: %v", err)
	}
	if ok, err := s.IsUnlocked(context.Background()); err != nil || ok {
		t.Fatalf("post-failure IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
}
