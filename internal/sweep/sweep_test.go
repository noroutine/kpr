package sweep

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var sweepNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func testCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = cancel
	return c
}

func duerow(repo, tag string, age time.Duration) policy.Row {
	return policy.Row{Repo: repo, Tag: tag, Digest: "sha256:abc",
		PushedAt: sweepNow.Add(-age), Due: true, Reason: "test"}
}

// fakeRegistry answers manifest DELETEs with the given status/body and
// counts them, so tests prove what the sweeper attempted — not just
// what it claims.
type fakeRegistry struct {
	srv   *httptest.Server
	calls atomic.Int64
}

func newFake(status int, body string) *fakeRegistry {
	f := &fakeRegistry{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			f.calls.Add(1)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return f
}

func (f *fakeRegistry) close() { f.srv.Close() }

func newSweeper(s store.Store, url string, dryRun bool) *Sweeper {
	return &Sweeper{
		Store:    s,
		Registry: registry.NewClient(url),
		DryRun:   dryRun,
		Now:      func() time.Time { return sweepNow },
	}
}

// Dry-run is implicit safety: the pass plans every due row, calls the
// registry zero times, and leaves rows marked for the armed run. If
// this fails, "safe by default" deletes — or plans something the armed
// run would not do.
func TestDryRunPlansWithoutDeleting(t *testing.T) {
	f := newFake(http.StatusAccepted, "")
	defer f.close()
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	sw := newSweeper(s, f.srv.URL, true)

	sum := sw.RunPass(testCtx(), "POST")
	if sum.Planned != 1 || sum.Performed != 0 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want {Planned:1}", sum)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("registry DELETEs = %d, want 0 in dry-run", n)
	}
	if due, _ := s.Due(testCtx()); len(due) != 1 {
		t.Errorf("dry-run resolved %d due rows, want 1 still marked", 1-len(due))
	}
	acts, _ := s.Activity(testCtx())
	if len(acts) != 1 || acts[0].Outcome != "planned" {
		t.Errorf("activity = %+v, want one planned outcome", acts)
	}
}

// The armed pass deletes due rows, resolves them from the store only
// on confirmation, and reports performed. If this fails, real deletes
// either don't happen or rows are dropped without confirmation.
func TestArmedPassDeletesAndResolves(t *testing.T) {
	f := newFake(http.StatusAccepted, "")
	defer f.close()
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
	sw := newSweeper(s, f.srv.URL, false)

	sum := sw.RunPass(testCtx(), "tick")
	if sum.Performed != 1 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want {Performed:1}", sum)
	}
	if all, _ := s.All(testCtx()); len(all) != 0 {
		t.Errorf("%d rows left after confirmed delete, want 0", len(all))
	}
	acts, _ := s.Activity(testCtx())
	if len(acts) != 1 || acts[0].Outcome != "deleted" {
		t.Errorf("activity = %+v, want one deleted outcome", acts)
	}
}

// The sweeper enforces the TTL expiry floor regardless of the mark: a
// due-marked row whose promise hasn't elapsed is skipped, never
// deleted. If this fails, a stale mark wipes a fresh push.
func TestFloorHoldsEarlyTTLMark(t *testing.T) {
	f := newFake(http.StatusAccepted, "")
	defer f.close()
	s := store.NewMemStore()
	r := duerow("scratch", "10m", 9*time.Minute)
	r.Reason = "ttl:10m elapsed"
	_ = s.Record(testCtx(), r)
	sw := newSweeper(s, f.srv.URL, false)

	sum := sw.RunPass(testCtx(), "tick")
	if sum.Performed != 0 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want no deletes before the promise", sum)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("registry DELETEs = %d, want 0 (floor holds)", n)
	}
	if due, _ := s.Due(testCtx()); len(due) != 1 {
		t.Error("floor-hold resolved the row, want it still due")
	}
}

// A registry 500 keeps the row due for next-tick retry and records
// the failure where `sweep`'s watch surfaces it. If this fails,
// failures are swallowed (row silently resolved) or crash the pass.
func TestRegistryFailureKeepsRowForRetry(t *testing.T) {
	f := newFake(http.StatusInternalServerError, "boom")
	defer f.close()
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
	sw := newSweeper(s, f.srv.URL, false)

	sum := sw.RunPass(testCtx(), "tick")
	if sum.Failed != 1 || sum.Performed != 0 {
		t.Errorf("summary = %+v, want {Failed:1}", sum)
	}
	if len(sum.Failures) != 1 {
		t.Errorf("failures = %v, want the surfaced error", sum.Failures)
	}
	if due, _ := s.Due(testCtx()); len(due) != 1 {
		t.Error("failed row resolved, want it still due for retry")
	}
	acts, _ := s.Activity(testCtx())
	if len(acts) != 1 || acts[0].Outcome != "failed" {
		t.Errorf("activity = %+v, want one failed outcome", acts)
	}
}

// An index-held manifest is untracked (row dropped, registry owns the
// child), never retried. If this fails, the sweeper hammers it forever.
func TestHeldManifestUntracks(t *testing.T) {
	f := newFake(http.StatusMethodNotAllowed, `{"errors":[{"code":"DENIED"}]}`)
	defer f.close()
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "child", 200*24*time.Hour))
	sw := newSweeper(s, f.srv.URL, false)

	sum := sw.RunPass(testCtx(), "tick")
	if sum.Untracked != 1 {
		t.Errorf("summary = %+v, want {Untracked:1}", sum)
	}
	if all, _ := s.All(testCtx()); len(all) != 0 {
		t.Errorf("%d rows left after untrack, want 0", len(all))
	}
}

// A trigger that finds the lock emits skip and stacks nothing: no
// deletes, no per-row outcomes, current stage reads skip. If this
// fails, concurrent passes delete together or a skip looks like work.
func TestLockedTriggerSkips(t *testing.T) {
	f := newFake(http.StatusAccepted, "")
	defer f.close()
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
	held, _ := s.AcquireLock(testCtx(), time.Minute)
	if !held {
		t.Fatal("could not pre-hold the lock")
	}
	defer func() { _ = s.ReleaseLock(testCtx()) }()
	sw := newSweeper(s, f.srv.URL, false)

	sum := sw.RunPass(testCtx(), "POST")
	if !sum.Skipped {
		t.Errorf("summary = %+v, want Skipped", sum)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("registry DELETEs = %d during skip, want 0", n)
	}
	cur, _ := s.GetCurrent(testCtx())
	if cur.Stage != StageSkip {
		t.Errorf("current stage = %q, want %q", cur.Stage, StageSkip)
	}
}

// Nothing due is a skip too, with the pass id visible in current so a
// watcher can tell "ran, nothing to do" from "never ran". If this
// fails, idle ticks masquerade as work (or leave no trace).
func TestEmptyDueSkips(t *testing.T) {
	f := newFake(http.StatusAccepted, "")
	defer f.close()
	s := store.NewMemStore()
	sw := newSweeper(s, f.srv.URL, false)

	sum := sw.RunPass(testCtx(), "tick")
	if !sum.Skipped {
		t.Errorf("summary = %+v, want Skipped", sum)
	}
	cur, _ := s.GetCurrent(testCtx())
	if cur.Stage != StageSkip || cur.PassID == "" {
		t.Errorf("current = %+v, want skip stage with a pass id", cur)
	}
}

// Without an injected clock the pass anchors at wall time: production
// has no test seam, so an overdue TTL row still sweeps. If this fails,
// the zero clock either panics the pass or freezes eligibility.
func TestRunPassDefaultsToWallClock(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	sw := &Sweeper{Store: s, DryRun: true}
	if sum := sw.RunPass(testCtx(), "tick"); sum.Planned != 1 {
		t.Errorf("summary = %+v, want 1 planned on wall clock", sum)
	}
}
