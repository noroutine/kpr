package storeops

import (
	"context"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/testing/fakes"
)

// Adopt pairs the store without minting: the read comes first, the
// identity follows the evidence. If Adopt is undefined, the ceremony
// has no use case behind it.
func TestAdoptPairsUnpairedStore(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	gen := stageServedGen(t, root)
	var out strings.Builder
	if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, "", ""); err != nil {
		t.Fatalf("adopt served lineage: %v", err)
	}
	ident, err := lock.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	served, _, err := sentinel.Read(context.Background(), fakes.FileAPI{Root: root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	if ident.ID != served.ID {
		t.Errorf("paired to %q, serves %q", ident.ID, served.ID)
	}
	if ident.BaselineGen != gen {
		t.Errorf("baseline %q, serves %q", ident.BaselineGen, gen)
	}
	if !strings.Contains(out.String(), ident.ID) {
		t.Errorf("ceremony names no identity:\n%s", out.String())
	}
}

// stageServedGen stages a fresh ID-bearing generation and returns
// its gen and ID: adopt tests need served evidence, not pairing.
func stageServedGen(t *testing.T, root string) (gen string) {
	t.Helper()
	gen = fakes.NewGenID(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: fakes.NewGenID(t), TS: now}); err != nil {
		t.Fatalf("stage served generation: %v", err)
	}
	return gen
}

func TestAdoptPinMatchPairs(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	stageServedGen(t, root)
	served, _, err := sentinel.Read(context.Background(), fakes.FileAPI{Root: root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	var out strings.Builder
	if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, served.ID, ""); err != nil {
		t.Fatalf("adopt with matching pin: %v", err)
	}
	ident, _ := lock.GetIdentity(context.Background())
	if ident.ID != served.ID {
		t.Errorf("paired to %q, pinned %q", ident.ID, served.ID)
	}
}

func TestAdoptPinMismatchRefuses(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	stageServedGen(t, root)
	served, _, err := sentinel.Read(context.Background(), fakes.FileAPI{Root: root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	pin := fakes.NewGenID(t)
	var out strings.Builder
	err = Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, pin, "")
	if err == nil {
		t.Fatal("adopt with mismatched pin succeeded, want refusal")
	} else if !strings.Contains(err.Error(), served.ID) || !strings.Contains(err.Error(), pin) {
		t.Errorf("refusal names neither side: %v", err)
	}
	ident, _ := lock.GetIdentity(context.Background())
	if ident.ID != "" {
		t.Errorf("refused adopt paired to %q", ident.ID)
	}
}

// errSentinelAPI fails every read: the registry-down stand-in for
// the ceremony.
type errSentinelAPI struct{ err error }

func (s errSentinelAPI) GetManifest(context.Context, string, string) ([]byte, error) {
	return nil, s.err
}

func (s errSentinelAPI) GetBlob(context.Context, string, string) ([]byte, error) {
	return nil, s.err
}

func TestAdoptAbsentRefusesWithoutIdent(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	var out strings.Builder
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, "", "")
	if err == nil {
		t.Fatal("adopt on silence succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "nothing served") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A registry that errors (not silence) refuses before pairing:
// unreadable is not absent. If this fails, a 500 reads as
// unpaired and adopts over an unknown lineage.
func TestAdoptUnreadableRefuses(t *testing.T) {
	_, _, lock := fakes.ProvenRun(t)
	var out strings.Builder
	err := Adopt(context.Background(), &out, errSentinelAPI{fakes.ErrTestStoreDown}, lock, lock, "", "")
	if err == nil {
		t.Fatal("adopt on erroring sentinel succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "sentinel unreadable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A pairing write failure surfaces, never silent success: the
// operator must know the lineage did not record. If this fails,
// an outage during adopt reads as paired.
func TestAdoptPrePairWriteFailureRefuses(t *testing.T) {
	_, root, _ := fakes.ProvenRun(t)
	ids := &fakes.FailIdentityStore{MemStore: store.NewMemStore(), Armed: true}
	var out strings.Builder
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root},
		ids, store.NewMemStore(), fakes.NewGenID(t), "")
	if err == nil {
		t.Fatal("adopt with failing lineage write succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unrecordable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

func TestAdoptAbsentBootstrapsWithIdent(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	pin := fakes.NewGenID(t)
	var out strings.Builder
	if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, pin, ""); err != nil {
		t.Fatalf("adopt with pin on silence: %v", err)
	}
	ident, _ := lock.GetIdentity(context.Background())
	if ident.ID != pin {
		t.Errorf("paired to %q, pinned %q", ident.ID, pin)
	}
}

func TestAdoptIdentityLessRefuses(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: fakes.NewGenID(t)}); err != nil {
		t.Fatalf("stage identity-less generation: %v", err)
	}
	var out strings.Builder
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, "", "")
	if err == nil {
		t.Fatal("adopt of an identity-less generation succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "identity-less") {
		t.Errorf("refusal names no cause: %v", err)
	}
	ident, _ := lock.GetIdentity(context.Background())
	if ident.ID != "" {
		t.Errorf("refused adopt paired to %q", ident.ID)
	}
}

// Re-pairing follows the served lineage and prunes the old epoch's
// sentinel rows (they would read as rollback evidence); rows of
// other repos are untouched. If this fails, an adopt carries stale
// evidence into the next verdict.
func TestAdoptRepairsForeignLineage(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	oldID := fakes.NewGenID(t)
	oldGen := fakes.NewGenID(t)
	ctx := context.Background()
	if err := lock.SetIdentity(ctx, store.Identity{ID: oldID, BaselineGen: oldGen}); err != nil {
		t.Fatalf("pair old lineage: %v", err)
	}
	for _, row := range []policy.Row{
		{Repo: sentinel.Repo, Tag: oldGen, PushedAt: time.Now().UTC().Add(-2 * time.Hour)},
		{Repo: sentinel.Repo, Tag: oldGen + "-sibling", PushedAt: time.Now().UTC().Add(-time.Hour)},
		{Repo: "other/repo", Tag: "v1", PushedAt: time.Now().UTC().Add(-time.Hour)},
	} {
		if err := lock.Record(ctx, row); err != nil {
			t.Fatalf("stage row: %v", err)
		}
	}
	stageServedGen(t, root)
	served, _, err := sentinel.Read(ctx, fakes.FileAPI{Root: root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	var out strings.Builder
	if err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, lock, lock, "", ""); err != nil {
		t.Fatalf("adopt foreign lineage: %v", err)
	}
	ident, _ := lock.GetIdentity(ctx)
	if ident.ID != served.ID {
		t.Errorf("paired to %q, serves %q", ident.ID, served.ID)
	}
	rows, err := lock.All(ctx)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	for _, r := range rows {
		if r.Repo == sentinel.Repo {
			t.Errorf("old-epoch sentinel row survived adopt: %+v", r)
		}
	}
	if len(rows) != 1 || rows[0].Repo != "other/repo" {
		t.Errorf("adopt pruned beyond its scope: %+v", rows)
	}
	if !strings.Contains(out.String(), "re-paired") {
		t.Errorf("ceremony warns nothing about the re-pairing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "pruned 2 rows") {
		t.Errorf("ceremony counts no prune:\n%s", out.String())
	}
}

// An unprunable old epoch refuses loudly: rows the ceremony
// cannot drop would read as rollback evidence in the next
// verdict. If this fails, a half-pruned adopt claims a clean
// epoch.
func TestAdoptPruneFailuresRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows fakes.FailRows
	}{
		{"unreadable rows", fakes.FailRows{AllErr: fakes.ErrTestStoreDown}},
		{"undeletable rows", fakes.FailRows{DelErr: fakes.ErrTestStoreDown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, root, lock := fakes.ProvenRun(t)
			ctx := context.Background()
			oldID := fakes.NewGenID(t)
			if err := lock.SetIdentity(ctx, store.Identity{ID: oldID}); err != nil {
				t.Fatalf("pair old lineage: %v", err)
			}
			if err := lock.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: "old",
				PushedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("stage row: %v", err)
			}
			stageServedGen(t, root)
			tc.rows.MemStore = lock
			var out strings.Builder
			err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, lock, tc.rows, "", "")
			if err == nil {
				t.Fatal("adopt with failing prune succeeded, want refusal")
			} else if !strings.Contains(err.Error(), "old epoch unprunable") {
				t.Errorf("refusal names no cause: %v", err)
			}
		})
	}
}

// A pairing write failure on re-pair or refresh surfaces, never
// silent success. If this fails, an outage mid-ceremony reads as
// re-paired.
func TestAdoptRepairWriteFailureRefuses(t *testing.T) {
	_, root, _ := fakes.ProvenRun(t)
	ctx := context.Background()
	gen := stageServedGen(t, root)
	served, _, err := sentinel.Read(ctx, fakes.FileAPI{Root: root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	ids := &fakes.FailIdentityStore{MemStore: store.NewMemStore()}
	if err := ids.SetIdentity(ctx, store.Identity{ID: served.ID, BaselineGen: gen}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	ids.Armed = true
	var out strings.Builder
	if err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, ids, store.NewMemStore(), "", ""); err == nil {
		t.Fatal("re-adopt with failing lineage write succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unrecordable") {
		t.Errorf("refusal names no cause: %v", err)
	}
	// Fresh store, served generation, failing write: the first
	// pairing records nothing and says so.
	fresh := &fakes.FailIdentityStore{MemStore: store.NewMemStore(), Armed: true}
	var freshOut strings.Builder
	if err := Adopt(ctx, &freshOut, fakes.FileAPI{Root: root}, fresh, store.NewMemStore(), "", ""); err == nil {
		t.Fatal("first pairing with failing lineage write succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unrecordable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

func TestAdoptAlreadyPairedRefreshesBaseline(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	id := fakes.NewGenID(t)
	first := fakes.NewGenID(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: first, ID: id, TS: now}); err != nil {
		t.Fatalf("stage first: %v", err)
	}
	ctx := context.Background()
	if err := lock.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: first}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	second := fakes.NewGenID(t)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: second, ID: id, TS: now}); err != nil {
		t.Fatalf("stage second: %v", err)
	}
	var out strings.Builder
	if err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, lock, lock, "", ""); err != nil {
		t.Fatalf("re-adopt same lineage: %v", err)
	}
	ident, _ := lock.GetIdentity(ctx)
	if ident.ID != id {
		t.Errorf("identity moved %q -> %q on a no-op adopt", id, ident.ID)
	}
	if ident.BaselineGen != second {
		t.Errorf("baseline %q, serves %q", ident.BaselineGen, second)
	}
}

func TestAdoptGenMismatchRefuses(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	gen := stageServedGen(t, root)
	var out strings.Builder
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, "", fakes.NewGenID(t))
	if err == nil {
		t.Fatal("adopt with mismatched --gen succeeded, want refusal")
	} else if !strings.Contains(err.Error(), gen) {
		t.Errorf("refusal names no served gen: %v", err)
	}
}
