package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

var cliNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func cliCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = cancel
	return c
}

func cliStore() *store.MemStore {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: cliNow.Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "latest", Digest: "sha256:b",
		PushedAt: cliNow.Add(-time.Hour)})
	_ = s.PushActivity(c, store.Outcome{Repo: "scratch", Tag: "10m",
		Reason: "ttl:10m elapsed", Outcome: "deleted", At: cliNow})
	return s
}

func liveRegistryClient(t *testing.T) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return registry.NewClient(srv.URL)
}

// status is the ssh-and-scripts banner: redis and registry reachability,
// sweeper arming, and counters from tracked state. If this fails,
// operators cannot tell a healthy keeper from a blind one.
func TestStatusRendersBannerAndCounters(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), liveRegistryClient(t), false); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	for _, want := range []string{"dry-run", "reachable", "tracked: 2", "due: 1", "performed: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
}

// A down registry reddens the banner but still reports: status is a
// reader, not a health gate. Redis down, in contrast, fails fast —
// without state every number would be a lie. If this fails, status
// either hides a down registry or invents numbers with no backend.
func TestStatusDegradesAndFailsFast(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), registry.NewClient("http://127.0.0.1:1"), false); err != nil {
		t.Fatalf("registry down must not fail status: %v", err)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("status hides dead registry:\n%s", out.String())
	}
	if err := runStatus(cliCtx(), io.Discard, errStore{}, liveRegistryClient(t), false); err == nil {
		t.Error("redis down succeeded, want a fast clear error")
	} else if !strings.Contains(err.Error(), "redis") {
		t.Errorf("error = %q, want it to name redis", err.Error())
	}
}

// plan prints pending candidates with reasons, optionally as JSON for
// piping into other tools. If this fails, the dry-run view and the
// reap view diverge — or scripts cannot consume the plan.
func TestPlanListsDueWithReasons(t *testing.T) {
	var out bytes.Buffer
	if err := runPlan(cliCtx(), &out, cliStore(), false); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if !strings.Contains(out.String(), "scratch:10m") || !strings.Contains(out.String(), "ttl:10m elapsed") {
		t.Errorf("plan missing candidate or reason:\n%s", out.String())
	}
	if strings.Contains(out.String(), "app:latest") {
		t.Errorf("plan lists unmarked :latest:\n%s", out.String())
	}

	var jout bytes.Buffer
	if err := runPlan(cliCtx(), &jout, cliStore(), true); err != nil {
		t.Fatalf("runPlan json: %v", err)
	}
	var decoded []map[string]string
	if err := json.Unmarshal(jout.Bytes(), &decoded); err != nil {
		t.Fatalf("plan --json is not JSON: %v\n%s", err, jout.String())
	}
	if len(decoded) != 1 || decoded[0]["reason"] == "" {
		t.Errorf("plan --json = %v, want one candidate with a reason", decoded)
	}
}

// Unarmed reap only prints the plan (same source as plan): nothing is
// marked, nothing will sweep. If this fails, the default run mutates —
// the implicit-dry-run promise broken at its most important site.
func TestReapDryRunPrintsWithoutMarking(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), false, cliNow); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	if !strings.Contains(out.String(), "scratch:10m") {
		t.Errorf("dry-run prints no plan:\n%s", out.String())
	}
	if due, _ := s.Due(context.Background()); len(due) != 0 {
		t.Errorf("dry-run marked %d rows, want 0", len(due))
	}
}

// Armed reap marks what the policies select — expired TTL here — and
// leaves :latest and fresh tags alone. If this fails, arming either
// marks nothing (sweep starves) or marks the tags it promised to keep.
func TestReapArmedMarksOnlySelected(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "20m",
		Digest: "sha256:f", PushedAt: cliNow.Add(-time.Minute)})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "latest",
		Digest: "sha256:b", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), true, cliNow); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	due, _ := s.Due(c)
	if len(due) != 1 || due[0].Tag != "10m" {
		t.Fatalf("due = %+v, want [scratch:10m]", due)
	}
	if due[0].Reason == "" {
		t.Error("marked row carries no reason")
	}
}

// sweep POSTs the console trigger and prints the pass summary. When
// the console cannot be reached it degrades to the tick backstop with
// the due count — never a bare connection error. If this fails, the
// puppeteer either cannot trigger or hides the backstop.
func TestSweepTriggersAndDegrades(t *testing.T) {
	sum := sweep.Summary{PassID: "p1", Trigger: "POST", Performed: 2, Failed: 1,
		Failures: []string{"app:v1: 500"}}
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sweep" || r.Method != http.MethodPost {
			t.Errorf("trigger = %s %s, want POST /api/sweep", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(sum)
	}))
	defer console.Close()

	var out bytes.Buffer
	if err := runSweep(cliCtx(), &out, cliStore(), console.URL); err != nil {
		t.Fatalf("runSweep: %v", err)
	}
	if !strings.Contains(out.String(), "2 performed") || !strings.Contains(out.String(), "1 failed") {
		t.Errorf("summary missing counts:\n%s", out.String())
	}

	var dout bytes.Buffer
	if err := runSweep(cliCtx(), &dout, cliStore(), "http://127.0.0.1:1"); err != nil {
		t.Fatalf("unreachable console must degrade, not fail: %v", err)
	}
	for _, want := range []string{"1 rows due", "not reached", "next tick"} {
		if !strings.Contains(dout.String(), want) {
			t.Errorf("degraded sweep missing %q:\n%s", want, dout.String())
		}
	}
}

// errStore fails every read: the redis-down stand-in.
type errStore struct{ store.Store }

func (errStore) Ping(context.Context) error { return errors.New("redis: connection refused") }
func (errStore) All(context.Context) ([]policy.Row, error) {
	return nil, errors.New("redis: connection refused")
}
