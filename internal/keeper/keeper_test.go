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
