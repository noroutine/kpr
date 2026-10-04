package keeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var keeperNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func keeperCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = cancel
	return c
}

func untaggedStage() *store.MemStore {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "gone", Tag: "v1", Digest: "sha256:a",
		PushedAt: keeperNow.Add(-200 * time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "kept", Tag: "v9", Digest: "sha256:b",
		PushedAt: keeperNow.Add(-200 * time.Hour)})
	return s
}

// The untagged policy reads the live catalog: a tracked tag gone
// from the registry past the grace period marks, a listed tag does
// not. If this fails, evaluation reasons about rows alone and either
// never marks untagged or marks the whole world.
func TestEvaluateUntaggedUsesCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tags := []string{}
		if strings.Contains(r.URL.Path, "/kept/") {
			tags = []string{"v9"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
	}))
	defer srv.Close()
	marked, err := EvaluatePolicy(keeperCtx(), untaggedStage(), registry.NewClient(srv.URL), keeperNow, nil, "untagged")
	if err != nil {
		t.Fatalf("EvaluatePolicy untagged: %v", err)
	}
	if len(marked) != 1 || marked[0].Repo != "gone" {
		t.Errorf("untagged = %v, want only gone:v1", marked)
	}
}

// stubCatalog answers the catalog probe without HTTP: evaluation must
// consume the registry through its port, not a concrete client. If
// this fails, the use case is still coupled to the transport.
type stubCatalog struct {
	tags map[string][]string
	err  error
}

func (f stubCatalog) Catalog(ctx context.Context, repo string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tags[repo], nil
}

// Untagged marks sort repo-major across repos: two gone repos with
// tag order opposing repo order, so a comparator falling through to
// tags sorts deterministically wrong. If this fails, multi-repo
// marks print in map order.
func TestEvaluateUntaggedSortsRepoMajor(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "zebra", Tag: "a", Digest: "sha256:a",
		PushedAt: keeperNow.Add(-200 * time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "apple", Tag: "z", Digest: "sha256:b",
		PushedAt: keeperNow.Add(-200 * time.Hour)})
	stub := stubCatalog{tags: map[string][]string{"zebra": {}, "apple": {}}}
	marked, err := EvaluatePolicy(keeperCtx(), s, stub, keeperNow, nil, "untagged")
	if err != nil {
		t.Fatalf("EvaluatePolicy untagged: %v", err)
	}
	if len(marked) != 2 || marked[0].Repo != "apple" || marked[1].Repo != "zebra" {
		t.Errorf("untagged = %v, want repo-major apple,zebra", marked)
	}
}

// The untagged selector marks only tags absent from the live catalog
// past grace: the stub lists v9 for kept and nothing for gone, so
// gone:v1 marks and kept:v9 stays. If this fails, evaluation reads
// the transport instead of its port — or the selector misfires.
func TestEvaluateUntaggedUsesStubCatalog(t *testing.T) {
	stub := stubCatalog{tags: map[string][]string{"gone": {}, "kept": {"v9"}}}
	marked, err := EvaluatePolicy(keeperCtx(), untaggedStage(), stub, keeperNow, nil, "untagged")
	if err != nil {
		t.Fatalf("EvaluatePolicy untagged: %v", err)
	}
	if len(marked) != 1 || marked[0].Repo != "gone" {
		t.Errorf("untagged = %v, want only gone:v1", marked)
	}
}

// Ghosts list what both witnesses agree is gone: the catalog 404s
// the repo and the fs holds no manifest dir. A repo the catalog
// fails on skips as unreadable (blip, not gone); a repo the fs
// still holds skips too; a repo the catalog lists but the fs lacks
// conflicts, never ghosts. Sentinel machinery never lists. Rows
// come back untouched with the evidence beside them — the listing
// judges nothing, the operator does. If this fails, ghosts either
// hide (operator acts blind) or the listing overclaims.
func TestListGhostsNeedsBothWitnesses(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	old := keeperNow.Add(-200 * 24 * time.Hour)
	for _, r := range []policy.Row{
		{Repo: "gone", Tag: "v1", Digest: "sha256:a", PushedAt: old},
		{Repo: "dropped", Tag: "v1", Digest: "sha256:b", PushedAt: old},
		{Repo: "held", Tag: "v1", Digest: "sha256:c", PushedAt: old},
		{Repo: "live", Tag: "v1", Digest: "sha256:d", PushedAt: old},
		{Repo: "flaky", Tag: "v1", Digest: "sha256:e", PushedAt: old},
		{Repo: "noroutine/kpr-sentinel", Tag: "v1", Digest: "sha256:f", PushedAt: old},
	} {
		_ = s.Record(c, r)
	}
	reg := ghostCatalog{
		tags:  map[string][]string{"live": {"v1"}, "held": {"v1"}, "dropped": {"other"}},
		gone:  map[string]bool{"gone": true, "noroutine/kpr-sentinel": true},
		flaky: map[string]bool{"flaky": true},
	}
	ghosts, conflicts, unreadable, err := ListGhosts(keeperCtx(), s, reg,
		map[string]bool{"held": true, "live": true}, ghostProof(t, s))
	if err != nil {
		t.Fatalf("ListGhosts: %v", err)
	}
	if len(ghosts) != 1 || ghosts[0].Row.Repo != "gone" {
		t.Fatalf("ghosts = %v, want [gone:v1]", ghosts)
	}
	if ghosts[0].Evidence == "" {
		t.Errorf("ghost evidence empty, want the catalog/fs account")
	}
	if len(conflicts) != 1 || conflicts[0] != "dropped" {
		t.Errorf("conflicts = %v, want [dropped]", conflicts)
	}
	if len(unreadable) != 1 || unreadable[0] != "flaky" {
		t.Errorf("unreadable = %v, want [flaky]", unreadable)
	}
	if _, _, _, err := ListGhosts(keeperCtx(), s, reg, nil, ghostProof(t, s)); err == nil {
		t.Error("ListGhosts(nil fs) succeeded, want refusal (one witness is not a listing)")
	}
}

// ghostCatalog 404s gone repos, fails flaky ones transiently, and
// lists tags for the rest — the three catalog answers ghosts join
// against the fs view.
type ghostCatalog struct {
	tags  map[string][]string
	gone  map[string]bool
	flaky map[string]bool
}

func (g ghostCatalog) Catalog(_ context.Context, repo string) ([]string, error) {
	if g.gone[repo] {
		return nil, &registry.StatusError{Op: "catalog " + repo, Status: 404}
	}
	if g.flaky[repo] {
		return nil, &registry.StatusError{Op: "catalog " + repo, Status: 500}
	}
	return g.tags[repo], nil
}

// A catalog-200 on an fs-absent repo is a witness conflict, not an
// agreement: the registry says the repo exists, the fs says it
// does not — usually a wrong volume, never a ghost. Conflicts
// surface in their own bucket and list nothing. If this fails, a
// misrooted fs turns live tags into deletion candidates.
func TestListGhostsSeparatesConflictBucket(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(keeperCtx(), policy.Row{Repo: "split", Tag: "v1", Digest: "sha256:a",
		PushedAt: keeperNow.Add(-200 * 24 * time.Hour)})
	reg := ghostCatalog{tags: map[string][]string{"split": {"other"}}}
	ghosts, conflicts, _, err := ListGhosts(keeperCtx(), s, reg, map[string]bool{"unrelated": true}, ghostProof(t, s))
	if err != nil {
		t.Fatalf("ListGhosts: %v", err)
	}
	if len(ghosts) != 0 {
		t.Errorf("ghosts = %v, want none (conflict is not agreement)", ghosts)
	}
	if len(conflicts) != 1 || conflicts[0] != "split" {
		t.Errorf("conflicts = %v, want [split]", conflicts)
	}
}

// An empty fs view proves nothing: a fresh volume and an unmounted
// one look identical, so tracked rows plus an empty set refuses
// instead of naming every 404 a ghost. If this fails, a dead mount
// reads as proof everything is gone.
func TestListGhostsRefusesEmptyFs(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(keeperCtx(), policy.Row{Repo: "gone", Tag: "v1", Digest: "sha256:a",
		PushedAt: keeperNow.Add(-200 * 24 * time.Hour)})
	reg := ghostCatalog{gone: map[string]bool{"gone": true}}
	if _, _, _, err := ListGhosts(keeperCtx(), s, reg, map[string]bool{}, ghostProof(t, s)); err == nil {
		t.Error("ListGhosts(empty fs, rows held) succeeded, want refusal")
	}
}

// The listing judges nothing, so it touches nothing: an already-due
// row comes back with its mark intact and the evidence beside it,
// never merged into it. If this fails, the view contradicts the
// store and hides rows the sweeper will act on.
func TestListGhostsKeepsRowState(t *testing.T) {
	s := store.NewMemStore()
	c := keeperCtx()
	_ = s.Record(c, policy.Row{Repo: "gone", Tag: "v1", Digest: "sha256:a",
		PushedAt: keeperNow.Add(-200 * 24 * time.Hour)})
	if err := s.MarkDue(c, "gone", "v1", "ttl:2160h0m0s elapsed"); err != nil {
		t.Fatalf("stage due mark: %v", err)
	}
	reg := ghostCatalog{gone: map[string]bool{"gone": true}}
	ghosts, _, _, err := ListGhosts(c, s, reg, map[string]bool{"other": true}, ghostProof(t, s))
	if err != nil {
		t.Fatalf("ListGhosts: %v", err)
	}
	if len(ghosts) != 1 {
		t.Fatalf("ghosts = %v, want [gone:v1]", ghosts)
	}
	if !ghosts[0].Row.Due || ghosts[0].Row.Reason != "ttl:2160h0m0s elapsed" {
		t.Errorf("row = %+v, want the stored mark untouched", ghosts[0].Row)
	}
	if ghosts[0].Evidence == "" {
		t.Error("evidence empty, want the catalog/fs account beside the row")
	}
}

// Without the same-store proof there is no listing: judging rows
// from a foreign or unpaired store is the ambiguity proofs exist
// to refuse. If this fails, the guard is decorative.
func TestListGhostsRefusesNilProof(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(keeperCtx(), policy.Row{Repo: "gone", Tag: "v1", Digest: "sha256:a",
		PushedAt: keeperNow.Add(-200 * 24 * time.Hour)})
	reg := ghostCatalog{gone: map[string]bool{"gone": true}}
	if _, _, _, err := ListGhosts(keeperCtx(), s, reg, map[string]bool{}, nil); err == nil {
		t.Error("ListGhosts(nil proof) succeeded, want refusal")
	}
}

// catalogGone pins the real adapter's 404 shape: a NAME_UNKNOWN
// listing names a gone repo, anything else is unknown. If this
// fails, the ghost branch reads the transport instead of the
// status it carries.
func TestCatalogGoneMatchesRealClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/gone/tags/list" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"NAME_UNKNOWN"}]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	reg := registry.NewClient(srv.URL)
	if _, err := reg.Catalog(keeperCtx(), "gone"); !catalogGone(err) {
		t.Errorf("catalogGone(404) = false, want true (real client shape)")
	}
	if _, err := reg.Catalog(keeperCtx(), "down"); catalogGone(err) {
		t.Errorf("catalogGone(500) = true, want false")
	}
}

// ghostProof mints on paired ground the way the cli does: the
// caller proves, ListGhosts checks. If minting fails here, the test
// ground (not the guard) is broken.
func ghostProof(t *testing.T, s *store.MemStore) proof.SameStore {
	t.Helper()
	if err := s.SetIdentity(keeperCtx(), store.Identity{ID: "test-id", BaselineGen: "gen-test"}); err != nil {
		t.Fatalf("stage identity: %v", err)
	}
	same, err := proof.Prover{
		Sentinel: ghostSentinel{}, Store: s,
		Now: func() time.Time { return keeperNow },
	}.Prove(keeperCtx())
	if err != nil {
		t.Fatalf("prove on paired ground: %v", err)
	}
	return same
}

// ghostSentinel serves one paired generation: id and baseline the
// proof judges the store against.
type ghostSentinel struct{}

func (ghostSentinel) GetManifest(context.Context, string, string) ([]byte, error) {
	return []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc"}}`), nil
}

func (ghostSentinel) GetBlob(context.Context, string, string) ([]byte, error) {
	return []byte(`{"v":1,"gen":"gen-test","id":"test-id","ts":"2026-09-27T12:00:00Z","writer":"kpr-gc"}`), nil
}

// An armed reap marks the evaluated rows due; unarmed it evaluates
// without marking. If this fails, dry-run marks or the armed run
// evaluates something it never records.
func TestReapArmedMarksUnarmedEvaluates(t *testing.T) {
	stage := func() *store.MemStore {
		s := store.NewMemStore()
		c := context.Background()
		_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
			PushedAt: keeperNow.Add(-time.Hour)})
		_ = s.Record(c, policy.Row{Repo: "app", Tag: "latest", Digest: "sha256:b",
			PushedAt: keeperNow.Add(-time.Hour)})
		return s
	}
	s := stage()
	marked, err := Reap(keeperCtx(), s, nil, keeperNow, nil, "all", true)
	if err != nil {
		t.Fatalf("armed reap: %v", err)
	}
	if len(marked) != 1 || marked[0].Tag != "10m" {
		t.Fatalf("armed reap = %v, want only scratch:10m", marked)
	}
	if due, _ := s.Due(keeperCtx()); len(due) != 1 {
		t.Errorf("armed reap left %d rows due, want 1 marked", len(due))
	}
	s = stage()
	marked, err = Reap(keeperCtx(), s, nil, keeperNow, nil, "all", false)
	if err != nil {
		t.Fatalf("unarmed reap: %v", err)
	}
	if len(marked) != 1 {
		t.Fatalf("unarmed reap = %v, want the evaluated row back", marked)
	}
	if due, _ := s.Due(keeperCtx()); len(due) != 0 {
		t.Errorf("unarmed reap marked %v, want nothing", due)
	}
}

// An unknown policy refuses and lists the valid ones, before anything
// marks. If this fails, a typo reaps the world or errors cryptically.
func TestReapUnknownPolicyRefuses(t *testing.T) {
	s := untaggedStage()
	_, err := Reap(keeperCtx(), s, nil, keeperNow, nil, "bogus", true)
	if err == nil {
		t.Fatal("reap bogus succeeded, want refusal")
	}
	for _, name := range PolicyNames {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q does not list policy %q", err, name)
		}
	}
	if due, _ := s.Due(keeperCtx()); len(due) != 0 {
		t.Errorf("refused reap marked %d rows, want nothing", len(due))
	}
}

// stubProber answers the banner probe without HTTP.
type stubProber struct{ err error }

func (f stubProber) Reachable(context.Context) error { return f.err }

// The plan inside FetchStatus sorts repo-major: two due rows with
// tag order opposing repo order pin the comparator's repo branch. If
// this fails, the dashboard plan prints in map order.
func TestFetchStatusPlanSortsRepoMajor(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "zebra", Tag: "a", Digest: "sha256:a",
		PushedAt: keeperNow, Due: true, Reason: "manual"})
	_ = s.Record(c, policy.Row{Repo: "apple", Tag: "z", Digest: "sha256:b",
		PushedAt: keeperNow, Due: true, Reason: "manual"})
	st := FetchStatus(keeperCtx(), s, stubProber{})
	if len(st.Plan) != 2 || st.Plan[0].Repo != "apple" || st.Plan[1].Repo != "zebra" {
		t.Errorf("plan = %+v, want repo-major apple,zebra", st.Plan)
	}
}

// Status counts every outcome kind once, for both readers: the plan
// sorts repo-major and every due row carries its reason. If this
// fails, `status` and the console diverge — or one of them miscounts.
func TestFetchStatusCountsOnceForBothReaders(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "b-repo", Tag: "z", Digest: "sha256:1",
		PushedAt: keeperNow, Due: true, Reason: "ttl elapsed"})
	_ = s.Record(c, policy.Row{Repo: "a-repo", Tag: "a", Digest: "sha256:2",
		PushedAt: keeperNow})
	for _, o := range []string{"deleted", "planned", "failed", "untracked"} {
		_ = s.PushActivity(c, store.Outcome{Repo: "r", Tag: "t",
			Reason: "x", Outcome: o, At: keeperNow})
	}
	st := FetchStatus(keeperCtx(), s, stubProber{})
	if !st.StoreOK || !st.RegistryOK {
		t.Fatalf("status = %+v, want both backends ok", st)
	}
	if st.Tracked != 2 || st.Due != 1 {
		t.Errorf("tracked/due = %d/%d, want 2/1", st.Tracked, st.Due)
	}
	if st.Performed != 1 || st.Planned != 1 || st.Failed != 1 || st.Untracked != 1 {
		t.Errorf("outcomes = %+v, want one of each kind", st)
	}
	if len(st.Plan) != 1 || st.Plan[0].Repo != "b-repo" {
		t.Errorf("plan = %+v, want the one due row", st.Plan)
	}
	if len(st.Activity) != 4 {
		t.Errorf("activity = %d entries, want 4", len(st.Activity))
	}
}

// Absent or down backends degrade to red/empty, never to invented
// numbers: the CLI fails on this, the console renders it. If this
// fails, a reader reports healthy state from nothing.
func TestFetchStatusDegradesWithoutBackends(t *testing.T) {
	st := FetchStatus(keeperCtx(), nil, nil)
	if st.StoreOK || st.RegistryOK || st.Tracked != 0 || st.Due != 0 {
		t.Errorf("nil status = %+v, want red and empty", st)
	}
	st = FetchStatus(keeperCtx(), store.NewMemStore(), stubProber{err: context.DeadlineExceeded})
	if !st.StoreOK || st.RegistryOK {
		t.Errorf("prober-down status = %+v, want store ok and registry red", st)
	}
}

// readFailStore answers Ping but loses the rows: a store that is up
// but unreadable. Status must flip red, not print a healthy empty.
type readFailStore struct {
	*store.MemStore
}

func (readFailStore) All(context.Context) ([]policy.Row, error) {
	return nil, errTestDown
}

func (readFailStore) Get(context.Context, string, string) (policy.Row, bool, error) {
	return policy.Row{}, false, errTestDown
}

// activityFailStore tracks fine but loses the activity ring: the
// rows print, the outcomes stay empty.
type activityFailStore struct {
	*store.MemStore
}

func (activityFailStore) Activity(context.Context) ([]store.Outcome, error) {
	return nil, errTestDown
}

// A store that pings but cannot list flips the status red: up is
// not readable. If this fails, a half-dead backend reports a
// healthy empty store.
func TestFetchStatusStoreUnreadableDegrades(t *testing.T) {
	st := FetchStatus(keeperCtx(), &readFailStore{store.NewMemStore()}, stubProber{})
	if st.StoreOK {
		t.Errorf("unreadable status = %+v, want StoreOK red", st)
	}
	if st.Tracked != 0 || st.Due != 0 || len(st.Plan) != 0 {
		t.Errorf("unreadable status = %+v, want no rows or plan", st)
	}
}

// Same-repo due rows sort by tag: the comparator's second leg must
// order deterministically, not inherit row order. If this fails,
// same-repo plans print in insertion order.
func TestFetchStatusPlanSortsTagsWithinRepo(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "solo", Tag: "z", Digest: "sha256:a",
		PushedAt: keeperNow, Due: true, Reason: "manual"})
	_ = s.Record(c, policy.Row{Repo: "solo", Tag: "a", Digest: "sha256:b",
		PushedAt: keeperNow, Due: true, Reason: "manual"})
	st := FetchStatus(keeperCtx(), s, stubProber{})
	if len(st.Plan) != 2 || st.Plan[0].Tag != "a" || st.Plan[1].Tag != "z" {
		t.Errorf("plan = %+v, want tag order a,z within solo", st.Plan)
	}
}

// Lost activity degrades the outcomes, not the rows: tracked and
// due print, the outcome counts stay zero. If this fails, a lost
// ring hides the rows with it.
func TestFetchStatusActivityLossKeepsRows(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:a",
		PushedAt: keeperNow, Due: true, Reason: "manual"})
	st := FetchStatus(keeperCtx(), &activityFailStore{s}, stubProber{})
	if !st.StoreOK || st.Tracked != 1 || st.Due != 1 || len(st.Plan) != 1 {
		t.Errorf("activity-loss status = %+v, want rows intact", st)
	}
	if st.Performed != 0 || st.Planned != 0 || st.Failed != 0 || st.Untracked != 0 {
		t.Errorf("activity-loss outcomes = %+v, want zeros", st)
	}
}

// One path evaluates one policy or all: name "all" fans out to the
// same joined evaluation the sweeper drives. If this fails, the CLI
// and the sweeper evaluate different marks.
func TestEvaluatePolicyAllFansOut(t *testing.T) {
	s := untaggedStage()
	one, err := EvaluatePolicy(keeperCtx(), s, stubCatalog{}, keeperNow, nil, "all")
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	all, err := EvaluatePolicies(keeperCtx(), s, stubCatalog{}, keeperNow, nil)
	if err != nil {
		t.Fatalf("policies: %v", err)
	}
	if len(one) != len(all) {
		t.Errorf("all = %d marks, policies = %d, want the same join", len(one), len(all))
	}
}

// Evaluating against dead state fails naming redis: no evaluation
// without rows. If this fails, a down store evaluates the empty
// world as clean.
func TestEvaluatePoliciesOnDeadStoreFails(t *testing.T) {
	if _, err := EvaluatePolicies(keeperCtx(), &readFailStore{store.NewMemStore()},
		stubCatalog{}, keeperNow, nil); err == nil {
		t.Error("policies on dead store succeeded, want an error")
	}
}

// A catalog that fails for one repo skips that repo's
// catalog-dependent selectors: rows-only selectors still apply. If
// this fails, one sick repo blinds every selector.
func TestFetchCatalogsSkipsFailedRepo(t *testing.T) {
	got := fetchCatalogs(keeperCtx(), stubCatalog{err: errTestDown},
		[]policy.Row{{Repo: "gone"}, {Repo: "kept"}})
	if len(got) != 0 {
		t.Errorf("catalogs = %v, want none (failed repo skipped)", got)
	}
}

// markFailStore evaluates fine but loses the mark write: redis down
// between the read and the write.
type markFailStore struct {
	*store.MemStore
}

func (markFailStore) MarkDue(context.Context, string, string, string) error {
	return errTestDown
}

// Marking against a dead store fails naming redis: the armed reap
// must not report marks it never wrote. If this fails, a down store
// reaps clean on paper.
func TestReapArmedOnDeadStoreFails(t *testing.T) {
	inner := store.NewMemStore()
	c := context.Background()
	_ = inner.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: keeperNow.Add(-time.Hour)})
	s := markFailStore{inner}
	if _, err := Reap(keeperCtx(), s, nil, keeperNow, nil, "ttl", true); err == nil {
		t.Error("armed reap on dead store succeeded, want an error")
	} else if !strings.Contains(err.Error(), "redis unreachable") {
		t.Errorf("refusal = %q, want redis named", err.Error())
	}
}

// sortMarks orders same-repo marks by tag: the direct unit pin for
// the comparator's second leg. If this fails, keep-N marks print in
// map order within a repo.
func TestSortMarksTagsWithinRepo(t *testing.T) {
	out := []policy.Row{{Repo: "solo", Tag: "z"}, {Repo: "solo", Tag: "a"}}
	sortMarks(out)
	if out[0].Tag != "a" || out[1].Tag != "z" {
		t.Errorf("marks = %v, want tag order a,z", out)
	}
}
