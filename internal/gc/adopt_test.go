package gc

import (
	"context"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Adopt pairs the store without minting: the read comes first, the
// identity follows the evidence. If Adopt is undefined, the ceremony
// has no use case behind it.
func TestAdoptPairsUnpairedStore(t *testing.T) {
	_, root, lock := stageProvenRun(t)
	gen := stageServedGen(t, root)
	var out strings.Builder
	if err := Adopt(context.Background(), &out, fileAPI{root}, lock, lock, "", ""); err != nil {
		t.Fatalf("adopt served lineage: %v", err)
	}
	ident, err := lock.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	served, _, err := sentinel.Read(context.Background(), fileAPI{root}, sentinel.Repo, sentinel.Tag)
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
	gen = newGenID(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: newGenID(t), TS: now}); err != nil {
		t.Fatalf("stage served generation: %v", err)
	}
	return gen
}

func TestAdoptPinMatchPairs(t *testing.T) {
	_, root, lock := stageProvenRun(t)
	stageServedGen(t, root)
	served, _, err := sentinel.Read(context.Background(), fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	var out strings.Builder
	if err := Adopt(context.Background(), &out, fileAPI{root}, lock, lock, served.ID, ""); err != nil {
		t.Fatalf("adopt with matching pin: %v", err)
	}
	ident, _ := lock.GetIdentity(context.Background())
	if ident.ID != served.ID {
		t.Errorf("paired to %q, pinned %q", ident.ID, served.ID)
	}
}

func TestAdoptPinMismatchRefuses(t *testing.T) {
	_, root, lock := stageProvenRun(t)
	stageServedGen(t, root)
	served, _, err := sentinel.Read(context.Background(), fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	pin := newGenID(t)
	var out strings.Builder
	err = Adopt(context.Background(), &out, fileAPI{root}, lock, lock, pin, "")
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

func TestAdoptAbsentRefusesWithoutIdent(t *testing.T) {
	_, root, lock := stageProvenRun(t)
	var out strings.Builder
	err := Adopt(context.Background(), &out, fileAPI{root}, lock, lock, "", "")
	if err == nil {
		t.Fatal("adopt on silence succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "nothing served") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

func TestAdoptAbsentBootstrapsWithIdent(t *testing.T) {
	_, root, lock := stageProvenRun(t)
	pin := newGenID(t)
	var out strings.Builder
	if err := Adopt(context.Background(), &out, fileAPI{root}, lock, lock, pin, ""); err != nil {
		t.Fatalf("adopt with pin on silence: %v", err)
	}
	ident, _ := lock.GetIdentity(context.Background())
	if ident.ID != pin {
		t.Errorf("paired to %q, pinned %q", ident.ID, pin)
	}
}

func TestAdoptIdentityLessRefuses(t *testing.T) {
	_, root, lock := stageProvenRun(t)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: newGenID(t)}); err != nil {
		t.Fatalf("stage identity-less generation: %v", err)
	}
	var out strings.Builder
	err := Adopt(context.Background(), &out, fileAPI{root}, lock, lock, "", "")
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
	_, root, lock := stageProvenRun(t)
	oldID := newGenID(t)
	oldGen := newGenID(t)
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
	served, _, err := sentinel.Read(ctx, fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	var out strings.Builder
	if err := Adopt(ctx, &out, fileAPI{root}, lock, lock, "", ""); err != nil {
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

func TestAdoptAlreadyPairedRefreshesBaseline(t *testing.T) {
	_, root, lock := stageProvenRun(t)
	id := newGenID(t)
	first := newGenID(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: first, ID: id, TS: now}); err != nil {
		t.Fatalf("stage first: %v", err)
	}
	ctx := context.Background()
	if err := lock.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: first}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	second := newGenID(t)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: second, ID: id, TS: now}); err != nil {
		t.Fatalf("stage second: %v", err)
	}
	var out strings.Builder
	if err := Adopt(ctx, &out, fileAPI{root}, lock, lock, "", ""); err != nil {
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
	_, root, lock := stageProvenRun(t)
	gen := stageServedGen(t, root)
	var out strings.Builder
	err := Adopt(context.Background(), &out, fileAPI{root}, lock, lock, "", newGenID(t))
	if err == nil {
		t.Fatal("adopt with mismatched --gen succeeded, want refusal")
	} else if !strings.Contains(err.Error(), gen) {
		t.Errorf("refusal names no served gen: %v", err)
	}
}
