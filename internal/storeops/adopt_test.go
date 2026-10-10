package storeops

import (
	"context"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
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
	if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, proof.Arm(true, false), "", ""); err != nil {
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
	if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, proof.Arm(true, false), served.ID, ""); err != nil {
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
	err = Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, proof.Arm(true, false), pin, "")
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
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, proof.Arm(true, false), "", "")
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
	err := Adopt(context.Background(), &out, errSentinelAPI{fakes.ErrTestStoreDown}, lock, lock, &stubAdoptRegistry{}, proof.Arm(true, false), "", "")
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
		ids, store.NewMemStore(), &stubAdoptRegistry{}, proof.Arm(true, false), fakes.NewGenID(t), "")
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
	if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, proof.Arm(true, false), pin, ""); err != nil {
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
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, proof.Arm(true, false), "", "")
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
// other repos are untouched. The catalog serves no fossils here,
// so the untag walk finds nothing — the zero-tag leg. If this
// fails, an adopt carries stale evidence into the next verdict.
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
	reg := &stubAdoptRegistry{
		repos: []string{sentinel.Repo},
		tags:  map[string][]string{sentinel.Repo: {sentinel.Tag}},
	}
	var out strings.Builder
	if err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, lock, lock, reg, proof.Arm(true, false), "", ""); err != nil {
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
	if !strings.Contains(out.String(), "untagged 0 tags") || !strings.Contains(out.String(), "pruned 2 rows") {
		t.Errorf("ceremony counts neither untag nor prune:\n%s", out.String())
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
			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := sentinel.Write(root, sentinel.Repo, "old",
				sentinel.Payload{V: 1, Gen: "old", ID: oldID, TS: now}); err != nil {
				t.Fatalf("stage old fossil tag: %v", err)
			}
			if err := lock.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: "old",
				PushedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("stage row: %v", err)
			}
			stageServedGen(t, root)
			tc.rows.MemStore = lock
			reg := &stubAdoptRegistry{
				repos: []string{sentinel.Repo},
				tags:  map[string][]string{sentinel.Repo: {sentinel.Tag, "old"}},
			}
			var out strings.Builder
			err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, lock, tc.rows, reg, proof.Arm(true, false), "", "")
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
	if err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, ids, store.NewMemStore(), &stubAdoptRegistry{}, proof.Arm(true, false), "", ""); err == nil {
		t.Fatal("re-adopt with failing lineage write succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unrecordable") {
		t.Errorf("refusal names no cause: %v", err)
	}
	// Fresh store, served generation, failing write: the first
	// pairing records nothing and says so.
	fresh := &fakes.FailIdentityStore{MemStore: store.NewMemStore(), Armed: true}
	var freshOut strings.Builder
	if err := Adopt(ctx, &freshOut, fakes.FileAPI{Root: root}, fresh, store.NewMemStore(), &stubAdoptRegistry{}, proof.Arm(true, false), "", ""); err == nil {
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
	if err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, proof.Arm(true, false), "", ""); err != nil {
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
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, nil, "", fakes.NewGenID(t))
	if err == nil {
		t.Fatal("adopt with mismatched --gen succeeded, want refusal")
	} else if !strings.Contains(err.Error(), gen) {
		t.Errorf("refusal names no served gen: %v", err)
	}
}

// stubAdoptRegistry serves a fixed catalog and records manifest
// deletes: the served-but-untracked sentinel tags the row walk
// cannot see. If this fails, adopt enumerates the registry through
// something other than the catalog port.
type stubAdoptRegistry struct {
	repos   []string
	tags    map[string][]string
	deleted []string
	err     error
	// catalogAllErr fails the namespace list, catalogErr the
	// per-repo list, outcome overrides the delete answer (held
	// registries refuse deletes without erroring).
	catalogAllErr error
	catalogErr    error
	outcome       string
}

func (s *stubAdoptRegistry) CatalogAll(context.Context) ([]string, error) {
	if s.catalogAllErr != nil {
		return nil, s.catalogAllErr
	}
	return s.repos, nil
}

func (s *stubAdoptRegistry) Catalog(_ context.Context, repo string) ([]string, error) {
	if s.catalogErr != nil {
		return nil, s.catalogErr
	}
	return s.tags[repo], nil
}

func (s *stubAdoptRegistry) DeleteManifest(_ context.Context, repo, ref string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.deleted = append(s.deleted, repo+":"+ref)
	if s.outcome != "" {
		return s.outcome, nil
	}
	return registry.OutcomeDeleted, nil
}

// stageForeignEpoch pairs an old identity, tracks its sentinel rows,
// and serves a new lineage whose floater plus one fossil tag the
// stub catalog answers: the re-pair's whole world.
func stageForeignEpoch(t *testing.T, root string, lock *store.MemStore, oldGen string) (oldID string) {
	t.Helper()
	ctx := context.Background()
	oldID = fakes.NewGenID(t)
	if err := lock.SetIdentity(ctx, store.Identity{ID: oldID, BaselineGen: oldGen}); err != nil {
		t.Fatalf("pair old lineage: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, oldGen,
		sentinel.Payload{V: 1, Gen: oldGen, ID: oldID, TS: now}); err != nil {
		t.Fatalf("stage old fossil tag: %v", err)
	}
	for _, row := range []policy.Row{
		{Repo: sentinel.Repo, Tag: oldGen, PushedAt: time.Now().UTC().Add(-2 * time.Hour)},
		{Repo: "other/repo", Tag: "v1", PushedAt: time.Now().UTC().Add(-time.Hour)},
	} {
		if err := lock.Record(ctx, row); err != nil {
			t.Fatalf("stage row: %v", err)
		}
	}
	return oldID
}

// A previewed re-pair changes nothing: the untag list narrates in
// the would-tense through the same evaluation arming executes,
// rows stay, the identity stays. If this fails, the preview
// promises what arming would not perform — or performs what the
// preview only promised.
func TestAdoptRepairPreviewsUntagList(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	oldGen := fakes.NewGenID(t)
	oldID := stageForeignEpoch(t, root, lock, oldGen)
	stageServedGen(t, root)
	reg := &stubAdoptRegistry{
		repos: []string{sentinel.Repo},
		tags:  map[string][]string{sentinel.Repo: {sentinel.Tag, oldGen}},
	}
	var out strings.Builder
	if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, reg, nil, "", ""); err != nil {
		t.Fatalf("previewed re-pair: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "would untag "+sentinel.Repo+":"+oldGen) {
		t.Errorf("preview names no fossil untag:\n%s", got)
	}
	if got := out.String(); strings.Contains(got, "would untag "+sentinel.Repo+":"+sentinel.Tag) {
		t.Errorf("preview would untag the served floater:\n%s", got)
	}
	rows, err := lock.All(context.Background())
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("preview pruned rows: %+v", rows)
	}
	if ident, _ := lock.GetIdentity(context.Background()); ident.ID != oldID {
		t.Errorf("preview moved identity to %q", ident.ID)
	}
	if len(reg.deleted) != 0 {
		t.Errorf("preview deleted tags: %v", reg.deleted)
	}
}

// An armed re-pair untags every served sentinel tag but the served
// digest's own, then prunes the old epoch's rows, then stamps the
// identity — in that order, each gate refusing before the next
// writes. The cut spans the whole machinery namespace: a fossil
// in a second kpr-* repo untags and its rows prune like the
// sentinel repo's own. If this fails, tagged garbage outlives the
// epoch cut.
func TestAdoptRepairUntagsPrunesStamps(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	ctx := context.Background()
	oldGen := fakes.NewGenID(t)
	oldID := stageForeignEpoch(t, root, lock, oldGen)
	sideRepo, sideTag := "noroutine/kpr-sidecar", fakes.NewGenID(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sideRepo, sideTag,
		sentinel.Payload{V: 1, Gen: sideTag, ID: oldID, TS: now}); err != nil {
		t.Fatalf("stage sidecar fossil tag: %v", err)
	}
	if err := lock.Record(ctx, policy.Row{Repo: sideRepo, Tag: sideTag,
		PushedAt: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatalf("stage sidecar row: %v", err)
	}
	stageServedGen(t, root)
	// A row on the served floater: the foreign epoch's tracking of
	// a tag the cut keeps. It prunes with the rest — kept tags do
	// not keep foreign rows.
	if err := lock.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: sentinel.Tag,
		PushedAt: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatalf("stage kept-tag row: %v", err)
	}
	served, _, err := sentinel.Read(ctx, fakes.FileAPI{Root: root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	reg := &stubAdoptRegistry{
		repos: []string{sentinel.Repo, sideRepo, "other/repo"},
		tags: map[string][]string{
			sentinel.Repo: {sentinel.Tag, oldGen},
			sideRepo:      {sideTag},
			"other/repo":  {"v1"},
		},
	}
	var out strings.Builder
	if err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, lock, lock, reg, proof.Arm(true, false), "", ""); err != nil {
		t.Fatalf("armed re-pair: %v", err)
	}
	wantDeleted := map[string]bool{
		sentinel.Repo + ":" + oldGen: true,
		sideRepo + ":" + sideTag:     true,
	}
	for _, gone := range reg.deleted {
		delete(wantDeleted, gone)
	}
	if len(wantDeleted) != 0 || len(reg.deleted) != 2 {
		t.Errorf("untagged %v, want both fossils", reg.deleted)
	}
	rows, err := lock.All(ctx)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Repo != "other/repo" {
		t.Errorf("adopt pruned beyond its scope: %+v", rows)
	}
	ident, _ := lock.GetIdentity(ctx)
	if ident.ID != served.ID {
		t.Errorf("paired to %q, serves %q", ident.ID, served.ID)
	}
	if got := out.String(); !strings.Contains(got, "untagged 2 tags") || !strings.Contains(got, "pruned 3 rows") {
		t.Errorf("ceremony counts neither untag nor prune:\n%s", got)
	}
}

// A failed sentinel delete refuses before any write: untagged
// tags with pruned rows would strand the epoch half-cut. If this
// fails, a partial untag claims a clean epoch.
func TestAdoptUntagFailureRefuses(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	oldGen := fakes.NewGenID(t)
	oldID := stageForeignEpoch(t, root, lock, oldGen)
	stageServedGen(t, root)
	reg := &stubAdoptRegistry{
		repos: []string{sentinel.Repo},
		tags:  map[string][]string{sentinel.Repo: {sentinel.Tag, oldGen}},
		err:   fakes.ErrTestStoreDown,
	}
	var out strings.Builder
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, reg, proof.Arm(true, false), "", "")
	if err == nil {
		t.Fatal("re-pair with failing untag succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "old epoch un-untaggable") {
		t.Errorf("refusal names no cause: %v", err)
	}
	rows, _ := lock.All(context.Background())
	if len(rows) != 2 {
		t.Errorf("refused re-pair pruned rows: %+v", rows)
	}
	if ident, _ := lock.GetIdentity(context.Background()); ident.ID != oldID {
		t.Errorf("refused re-pair moved identity to %q", ident.ID)
	}
}

// A held delete refuses like a failed one: the registry said no,
// so the epoch is not cut. If this fails, a registry-side hold
// reads as a completed untag.
func TestAdoptHeldUntagRefuses(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	oldGen := fakes.NewGenID(t)
	oldID := stageForeignEpoch(t, root, lock, oldGen)
	stageServedGen(t, root)
	reg := &stubAdoptRegistry{
		repos:   []string{sentinel.Repo},
		tags:    map[string][]string{sentinel.Repo: {sentinel.Tag, oldGen}},
		outcome: registry.OutcomeHeld,
	}
	var out strings.Builder
	err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, reg, proof.Arm(true, false), "", "")
	if err == nil {
		t.Fatal("re-pair with held untag succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "held the delete") {
		t.Errorf("refusal names no cause: %v", err)
	}
	rows, _ := lock.All(context.Background())
	if len(rows) != 2 {
		t.Errorf("refused re-pair pruned rows: %+v", rows)
	}
	if ident, _ := lock.GetIdentity(context.Background()); ident.ID != oldID {
		t.Errorf("refused re-pair moved identity to %q", ident.ID)
	}
}

// An unlistable catalog refuses before any write: fossils nobody
// can enumerate cannot be cut. If this fails, a blind re-pair
// claims the namespace it never walked.
func TestAdoptUnlistableCatalogRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  *stubAdoptRegistry
	}{
		{"namespace list fails", &stubAdoptRegistry{catalogAllErr: fakes.ErrTestStoreDown}},
		{"repo list fails", &stubAdoptRegistry{
			repos:      []string{sentinel.Repo},
			catalogErr: fakes.ErrTestStoreDown,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, root, lock := fakes.ProvenRun(t)
			oldGen := fakes.NewGenID(t)
			oldID := stageForeignEpoch(t, root, lock, oldGen)
			stageServedGen(t, root)
			var out strings.Builder
			err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, tc.reg, proof.Arm(true, false), "", "")
			if err == nil {
				t.Fatal("re-pair over unlistable catalog succeeded, want refusal")
			} else if !strings.Contains(err.Error(), "sentinels unlistable") {
				t.Errorf("refusal names no cause: %v", err)
			}
			rows, _ := lock.All(context.Background())
			if len(rows) != 2 {
				t.Errorf("refused re-pair pruned rows: %+v", rows)
			}
			if ident, _ := lock.GetIdentity(context.Background()); ident.ID != oldID {
				t.Errorf("refused re-pair moved identity to %q", ident.ID)
			}
		})
	}
}

// corruptTagAPI serves garbage for one tag: the fossil nobody can
// resolve.
type corruptTagAPI struct {
	fakes.FileAPI
	tag string
}

func (a corruptTagAPI) GetManifest(ctx context.Context, repo, tag string) ([]byte, error) {
	if tag == a.tag {
		return []byte("not a manifest"), nil
	}
	return a.FileAPI.GetManifest(ctx, repo, tag)
}

// An unresolvable fossil refuses: untagging what the ceremony
// cannot read would cut blind. A tag vanished between catalog and
// read is already gone (skipped); anything else unreadable stops
// the cut. If this fails, corruption in the machinery namespace
// passes silent.
func TestAdoptUnresolvableFossilRefuses(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	oldGen := fakes.NewGenID(t)
	oldID := stageForeignEpoch(t, root, lock, oldGen)
	stageServedGen(t, root)
	api := corruptTagAPI{FileAPI: fakes.FileAPI{Root: root}, tag: oldGen}
	reg := &stubAdoptRegistry{
		repos: []string{sentinel.Repo},
		tags:  map[string][]string{sentinel.Repo: {sentinel.Tag, oldGen, "phantom"}},
	}
	var out strings.Builder
	err := Adopt(context.Background(), &out, api, lock, lock, reg, proof.Arm(true, false), "", "")
	if err == nil {
		t.Fatal("re-pair over corrupt fossil succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if ident, _ := lock.GetIdentity(context.Background()); ident.ID != oldID {
		t.Errorf("refused re-pair moved identity to %q", ident.ID)
	}
}

// A phantom tag (cataloged, vanished before the read) skips: the
// untag it names is already done. If this fails, a catalog/read
// race refuses a cut that already converged.
func TestAdoptPhantomFossilSkips(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	oldGen := fakes.NewGenID(t)
	stageForeignEpoch(t, root, lock, oldGen)
	stageServedGen(t, root)
	reg := &stubAdoptRegistry{
		repos: []string{sentinel.Repo},
		tags:  map[string][]string{sentinel.Repo: {sentinel.Tag, oldGen, "phantom"}},
	}
	var out strings.Builder
	if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, reg, proof.Arm(true, false), "", ""); err != nil {
		t.Fatalf("re-pair over phantom tag: %v", err)
	}
	for _, gone := range reg.deleted {
		if strings.HasSuffix(gone, ":phantom") {
			t.Errorf("untagged a phantom tag: %v", reg.deleted)
		}
	}
	if len(reg.deleted) != 1 {
		t.Errorf("untagged %v, want only the fossil", reg.deleted)
	}
}

// A dead output fails the ceremony: reporting a pairing nobody
// saw is silent success. If this fails, narration errors go
// quiet.
func TestAdoptPreviewDeadWriterRefuses(t *testing.T) {
	_, root, lock := fakes.ProvenRun(t)
	oldGen := fakes.NewGenID(t)
	stageForeignEpoch(t, root, lock, oldGen)
	stageServedGen(t, root)
	reg := &stubAdoptRegistry{
		repos: []string{sentinel.Repo},
		tags:  map[string][]string{sentinel.Repo: {sentinel.Tag, oldGen}},
	}
	var fail failWriter
	if err := Adopt(context.Background(), &fail, fakes.FileAPI{Root: root}, lock, lock, reg, nil, "", ""); err == nil {
		t.Error("preview over dead writer succeeded, want the write failure")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, fakes.ErrTestStoreDown }

// Previews change nothing on the silent and paired paths either:
// would-pair and would-refresh narrate through the same reads
// arming writes through. If this fails, a preview performs or an
// armed run surprises.
func TestAdoptPreviewsChangeNothing(t *testing.T) {
	t.Run("pre-pair", func(t *testing.T) {
		_, root, lock := fakes.ProvenRun(t)
		pin := fakes.NewGenID(t)
		var out strings.Builder
		if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, nil, pin, ""); err != nil {
			t.Fatalf("previewed pre-pair: %v", err)
		}
		if !strings.Contains(out.String(), "would pair to "+pin) {
			t.Errorf("preview names no would-pairing:\n%s", out.String())
		}
		if ident, _ := lock.GetIdentity(context.Background()); ident.ID != "" {
			t.Errorf("preview paired to %q", ident.ID)
		}
	})
	t.Run("refresh", func(t *testing.T) {
		_, root, lock := fakes.ProvenRun(t)
		ctx := context.Background()
		stageServedGen(t, root)
		served, _, err := sentinel.Read(ctx, fakes.FileAPI{Root: root}, sentinel.Repo, sentinel.Tag)
		if err != nil {
			t.Fatalf("read served: %v", err)
		}
		first := fakes.NewGenID(t)
		if err := lock.SetIdentity(ctx, store.Identity{ID: served.ID, BaselineGen: first}); err != nil {
			t.Fatalf("pair: %v", err)
		}
		var out strings.Builder
		if err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, nil, "", ""); err != nil {
			t.Fatalf("previewed refresh: %v", err)
		}
		if !strings.Contains(out.String(), "would refresh baseline") {
			t.Errorf("preview names no would-refresh:\n%s", out.String())
		}
		ident, _ := lock.GetIdentity(ctx)
		if ident.BaselineGen != first {
			t.Errorf("preview moved baseline to %q", ident.BaselineGen)
		}
	})
	t.Run("fresh pair", func(t *testing.T) {
		_, root, lock := fakes.ProvenRun(t)
		gen := stageServedGen(t, root)
		served, _, err := sentinel.Read(context.Background(), fakes.FileAPI{Root: root}, sentinel.Repo, sentinel.Tag)
		if err != nil {
			t.Fatalf("read served: %v", err)
		}
		var out strings.Builder
		if err := Adopt(context.Background(), &out, fakes.FileAPI{Root: root}, lock, lock, &stubAdoptRegistry{}, nil, "", ""); err != nil {
			t.Fatalf("previewed pairing: %v", err)
		}
		if !strings.Contains(out.String(), "would pair to "+served.ID+" at generation "+gen) {
			t.Errorf("preview names no would-pairing:\n%s", out.String())
		}
		if ident, _ := lock.GetIdentity(context.Background()); ident.ID != "" {
			t.Errorf("preview paired to %q", ident.ID)
		}
	})
}

// A lineage outage at the stamp refuses: untagged tags with pruned
// rows and no identity strand the epoch half-cut. If this fails, a
// stamp failure after successful deletes claims paired.
func TestAdoptRepairStampFailureRefuses(t *testing.T) {
	_, root, _ := fakes.ProvenRun(t)
	ctx := context.Background()
	oldGen := fakes.NewGenID(t)
	oldID := fakes.NewGenID(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, oldGen,
		sentinel.Payload{V: 1, Gen: oldGen, ID: oldID, TS: now}); err != nil {
		t.Fatalf("stage old fossil tag: %v", err)
	}
	stageServedGen(t, root)
	rows := store.NewMemStore()
	if err := rows.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: oldGen,
		PushedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("stage row: %v", err)
	}
	ids := &fakes.FailIdentityStore{MemStore: store.NewMemStore()}
	if err := ids.SetIdentity(ctx, store.Identity{ID: oldID, BaselineGen: oldGen}); err != nil {
		t.Fatalf("pair old lineage: %v", err)
	}
	ids.Armed = true
	reg := &stubAdoptRegistry{
		repos: []string{sentinel.Repo},
		tags:  map[string][]string{sentinel.Repo: {sentinel.Tag, oldGen}},
	}
	var out strings.Builder
	err := Adopt(ctx, &out, fakes.FileAPI{Root: root}, ids, rows, reg, proof.Arm(true, false), "", "")
	if err == nil {
		t.Fatal("re-pair with failing stamp succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unrecordable") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if len(reg.deleted) != 1 {
		t.Errorf("refused re-pair untagged %v before failing", reg.deleted)
	}
}
