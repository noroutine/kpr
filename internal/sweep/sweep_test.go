package sweep

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// fakeRegistry answers manifest DELETEs with the given status/body,
// counting them and remembering every deleted reference, so tests
// prove what the sweeper attempted — not just what it claims.
type fakeRegistry struct {
	srv   *httptest.Server
	calls atomic.Int64
	mu    sync.Mutex
	refs  []string
}

func newFake(status int, body string) *fakeRegistry {
	f := &fakeRegistry{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			f.calls.Add(1)
			f.mu.Lock()
			f.refs = append(f.refs, r.URL.Path)
			f.mu.Unlock()
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
		Sentinel: pairGround(s),
		DryRun:   dryRun,
		Now:      func() time.Time { return sweepNow },
	}
}

// stubRegistry answers DeleteManifest without HTTP: the sweeper must
// consume the registry through a small port, not a concrete client.
// If this fails, the sweep use case is still coupled to the transport.
type stubRegistry struct {
	mu      sync.Mutex
	refs    []string
	outcome string
	// failFirst fails the first DeleteManifest call with err and
	// succeeds the rest, whichever ref arrives first: a mid-pass
	// failure beside a success in one pass, independent of store
	// order, no stateful server needed.
	failFirst bool
	err       error
}

func (f *stubRegistry) DeleteManifest(ctx context.Context, repo, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, repo+"@"+ref)
	if f.failFirst && len(f.refs) == 1 {
		return "", f.err
	}
	return f.outcome, nil
}

// staleDueStore replays the pass-start due list while the wrapped
// store moves on: the mid-pass push the re-read exists for,
// deterministically. Get delegates to live state; Due stays frozen.
type staleDueStore struct {
	store.Store
	stale []policy.Row
}

func (s staleDueStore) Due(context.Context) ([]policy.Row, error) { return s.stale, nil }

// Progress reports the running summary after every settled row:
// the last report equals the returned summary, so a live line
// converges instead of jumping. If this fails, the repaint
// shows stale counts while the pass moves on.
func TestRunPassProgressConverges(t *testing.T) {
	s := store.NewMemStore()
	c := testCtx()
	_ = s.Record(c, duerow("app", "v1", time.Hour))
	_ = s.Record(c, duerow("app", "v2", time.Hour))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	var got []Summary
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s),
		Now:      func() time.Time { return sweepNow },
		Progress: func(sum Summary) { got = append(got, sum) }}
	sum := sw.RunPass(c, "test")
	if len(got) != 3 {
		t.Fatalf("progress reports = %d, want initial + 2 settled rows", len(got))
	}
	last := got[len(got)-1]
	if last.PassID != sum.PassID || last.Performed != sum.Performed ||
		last.Planned != sum.Planned || last.Failed != sum.Failed || last.Untracked != sum.Untracked {
		t.Errorf("last progress = %+v, want %+v", last, sum)
	}
}

// A push landing mid-pass clears the mark in the store, but the
// pass already holds the stale copy: the pre-delete re-read must
// see the cleared row and skip it, so the fresh manifest survives
// and its tracking with it. If this fails, the sweeper deletes
// off pass-start state and a re-push between mark and sweep wipes
// the live image.
func TestRunPassRereadsDueBeforeDelete(t *testing.T) {
	s := store.NewMemStore()
	c := testCtx()
	if err := s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:old",
		PushedAt: sweepNow.Add(-2 * time.Hour), Due: true, Reason: "test"}); err != nil {
		t.Fatalf("stage due: %v", err)
	}
	stale, err := s.Due(c)
	if err != nil {
		t.Fatalf("stage due: %v", err)
	}
	// The mid-pass push: strictly newer, so Record clears the mark.
	if err := s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:new",
		PushedAt: sweepNow.Add(-time.Hour), Actor: "kpr-receiver"}); err != nil {
		t.Fatalf("stage push: %v", err)
	}
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: staleDueStore{s, stale}, Registry: stub,
		Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	sum := sw.RunPass(c, "test")
	if sum.Performed != 0 {
		t.Errorf("summary = %+v, want 0 performed (mark cleared mid-pass)", sum)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.refs) != 0 {
		t.Errorf("deleted %v, want no registry call (fresh row is not due)", stub.refs)
	}
	kept, ok, err := s.Get(c, "app", "v1")
	if err != nil || !ok || kept.Digest != "sha256:new" || kept.Due {
		t.Errorf("Get = %+v, %v, %v, want the fresh row held, tracking intact", kept, ok, err)
	}
	acts, err := s.Activity(c)
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	skipped := false
	for _, a := range acts {
		if a.Repo == "app" && a.Tag == "v1" && a.Outcome == "skipped" {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("activity = %+v, want a skipped outcome for app:v1", acts)
	}
}

// A row untracked mid-pass (rm beats the sweep to it) re-reads
// absent: the pass skips without a registry call. If this fails,
// the zero Row's Due=false only accidentally saves it — or not at
// all — and the pass deletes for a row that no longer exists.
func TestRunPassSkipsUntrackedRow(t *testing.T) {
	s := store.NewMemStore()
	c := testCtx()
	if err := s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:old",
		PushedAt: sweepNow.Add(-2 * time.Hour), Due: true, Reason: "test"}); err != nil {
		t.Fatalf("stage due: %v", err)
	}
	stale, err := s.Due(c)
	if err != nil {
		t.Fatalf("stage due: %v", err)
	}
	if err := s.Delete(c, "app", "v1"); err != nil {
		t.Fatalf("stage untrack: %v", err)
	}
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: staleDueStore{s, stale}, Registry: stub,
		Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	if sum := sw.RunPass(c, "test"); sum.Performed != 0 {
		t.Errorf("summary = %+v, want 0 performed (row untracked mid-pass)", sum)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.refs) != 0 {
		t.Errorf("deleted %v, want no registry call (row is gone)", stub.refs)
	}
}

// A re-read the store cannot serve fails the row loudly: Failed,
// named in Failures, resolved failed, no registry call. If this
// fails, dropping the error's continue deletes off the stale copy
// — the exact failure the re-read exists to prevent.
func TestRunPassFailsRowOnUnreadableState(t *testing.T) {
	s := store.NewMemStore()
	c := testCtx()
	if err := s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:old",
		PushedAt: sweepNow.Add(-2 * time.Hour), Due: true, Reason: "test"}); err != nil {
		t.Fatalf("stage due: %v", err)
	}
	stale, err := s.Due(c)
	if err != nil {
		t.Fatalf("stage due: %v", err)
	}
	boom := errors.New("store down")
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: getErrStore{staleDueStore{s, stale}, boom}, Registry: stub,
		Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	sum := sw.RunPass(c, "test")
	if sum.Failed != 1 || len(sum.Failures) != 1 || !strings.Contains(sum.Failures[0], "app:v1") {
		t.Errorf("summary = %+v, want 1 failed naming app:v1", sum)
	}
	// A failed row still counts as attempted: Done must include
	// it, or watchers read a pass that never finishes its plan.
	if cur, _ := s.GetCurrent(c); cur.Done != 1 || cur.Due != 1 {
		t.Errorf("current = %+v, want Due 1 Done 1 (failure counts)", cur)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.refs) != 0 {
		t.Errorf("deleted %v, want no registry call (state unreadable)", stub.refs)
	}
}

// getErrStore fails only the re-read: Due replays the pass-start
// snapshot while Get refuses, the unreadable-state mid-pass.
type getErrStore struct {
	staleDueStore
	err error
}

func (s getErrStore) Get(context.Context, string, string) (policy.Row, bool, error) {
	return policy.Row{}, false, s.err
}

// A still-due row under a moved digest skips: the pass evaluated
// the old digest, and deleting the fresh one is the dangerous
// option. The next pass re-evaluates against current state. If
// this fails, the substitution it pins is untested in both
// directions and the pass deletes what it never approved.
func TestRunPassSkipsMovedDigest(t *testing.T) {
	s := store.NewMemStore()
	c := testCtx()
	if err := s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:old",
		PushedAt: sweepNow.Add(-2 * time.Hour), Due: true, Reason: "test"}); err != nil {
		t.Fatalf("stage due: %v", err)
	}
	stale, err := s.Due(c)
	if err != nil {
		t.Fatalf("stage due: %v", err)
	}
	// Synthetic (no production writer holds Due across a digest
	// move): the mark survives, the digest does not.
	if err := s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:new",
		PushedAt: sweepNow.Add(-time.Hour), Due: true, Reason: "test"}); err != nil {
		t.Fatalf("stage moved digest: %v", err)
	}
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: staleDueStore{s, stale}, Registry: stub,
		Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	if sum := sw.RunPass(c, "test"); sum.Performed != 0 {
		t.Errorf("summary = %+v, want 0 performed (digest moved under the mark)", sum)
	}
	// A skipped row still counts as attempted: Done must include
	// it, or watchers read a pass that never finishes its plan.
	if cur, _ := s.GetCurrent(c); cur.Done != 1 || cur.Due != 1 {
		t.Errorf("current = %+v, want Due 1 Done 1 (skip counts)", cur)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.refs) != 0 {
		t.Errorf("deleted %v, want no registry call (approved digest is stale)", stub.refs)
	}
}

// An armed pass deletes through the Registry port: the stub records
// the digest reference and the confirmed row leaves the store. If
// this fails, the sweep use case bypasses its port or deletes by tag
// instead of digest.
func TestSweeperDeletesViaStubRegistry(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	sum := sw.RunPass(testCtx(), "test")
	if sum.Performed != 1 {
		t.Errorf("summary = %+v, want 1 performed", sum)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.refs) != 1 || stub.refs[0] != "app@sha256:abc" {
		t.Errorf("deleted refs = %v, want [app@sha256:abc]", stub.refs)
	}
	if all, _ := s.All(testCtx()); len(all) != 0 {
		t.Errorf("%d rows survived a confirmed delete, want 0", len(all))
	}
}

// Dry-run is implicit safety: the pass plans every due row, counts
// each planned row as performed (everything short of the delete),
// calls the registry zero times, and leaves rows marked for the
// armed run. If this fails, "safe by default" deletes — or plans
// something the armed run would not do.
func TestDryRunPlansWithoutDeleting(t *testing.T) {
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), DryRun: true, Now: func() time.Time { return sweepNow }}

	sum := sw.RunPass(testCtx(), "POST")
	if sum.Planned != 1 || sum.Performed != 1 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want {Planned:1 Performed:1}", sum)
	}
	if n := len(stub.refs); n != 0 {
		t.Errorf("registry DELETEs = %d, want 0 in dry-run", n)
	}
	if due, _ := s.Due(testCtx()); len(due) != 1 {
		t.Errorf("dry-run resolved %d due rows, want 1 still marked", 1-len(due))
	}
	acts, _ := s.Activity(testCtx())
	if len(acts) != 1 || acts[0].Outcome != "planned" {
		t.Errorf("activity = %+v, want one planned outcome", acts)
	}
	if len(acts) == 1 && acts[0].Actor != "kpr-sweep" {
		t.Errorf("activity actor = %q, want the sweeper named", acts[0].Actor)
	}
	if len(acts) == 1 && acts[0].Trigger != "POST" {
		t.Errorf("activity trigger = %q, want the pass trigger", acts[0].Trigger)
	}
	// The pass counts every attempted row: watchers read Done to tell
	// progress from a stall. If this fails, the run-state undercounts.
	if cur, _ := s.GetCurrent(testCtx()); cur.Done != 1 || cur.Due != 1 {
		t.Errorf("current = %+v, want Due 1 Done 1", cur)
	}
}

// Deletes go by digest, never by tag: registries in the distribution:3
// line reject tag deletes outright (405 UNSUPPORTED), while a digest
// delete is confirmed and universal. A digest-less row falls back to
// its tag (old registries accept it; new ones fail visibly and the row
// stays due). If this fails, every armed pass 405s on modern
// registries and nothing is ever collected.
func TestArmedPassDeletesByDigest(t *testing.T) {
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}

	if sum := sw.RunPass(testCtx(), "tick"); sum.Performed != 1 {
		t.Fatalf("summary = %+v, want Performed 1", sum)
	}
	if ref := stub.refs[len(stub.refs)-1]; !strings.HasSuffix(ref, "sha256:abc") {
		t.Errorf("deleted ref = %q, want the row digest", ref)
	}

	tagOnly := duerow("app", "v9", 200*24*time.Hour)
	tagOnly.Digest = ""
	_ = s.Record(testCtx(), tagOnly)
	if sum := sw.RunPass(testCtx(), "tick"); sum.Performed != 1 {
		t.Fatalf("summary = %+v, want the digest-less row attempted too", sum)
	}
	if ref := stub.refs[len(stub.refs)-1]; !strings.HasSuffix(ref, "v9") {
		t.Errorf("deleted ref = %q, want tag fallback for digest-less rows", ref)
	}
}

// The armed pass deletes due rows end to end through the real client:
// this is the one integration test that keeps a loopback registry, so
// the port and the adapter stay proven together. Rows resolve from
// the store only on confirmation, and the pass reports performed. If
// this fails, real deletes either don't happen or rows are dropped
// without confirmation.
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
	if cur, _ := s.GetCurrent(testCtx()); cur.Done != 1 || cur.Due != 1 {
		t.Errorf("current = %+v, want Due 1 Done 1", cur)
	}
}

// The sweeper enforces the TTL expiry floor regardless of the mark: a
// due-marked row whose promise hasn't elapsed is skipped, never
// deleted. If this fails, a stale mark wipes a fresh push.
func TestFloorHoldsEarlyTTLMark(t *testing.T) {
	s := store.NewMemStore()
	r := duerow("scratch", "10m", 9*time.Minute)
	r.Reason = "ttl:10m elapsed"
	_ = s.Record(testCtx(), r)
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}

	sum := sw.RunPass(testCtx(), "tick")
	if sum.Performed != 0 || sum.Failed != 0 {
		t.Errorf("summary = %+v, want no deletes before the promise", sum)
	}
	if n := len(stub.refs); n != 0 {
		t.Errorf("registry DELETEs = %d, want 0 (floor holds)", n)
	}
	if due, _ := s.Due(testCtx()); len(due) != 1 {
		t.Error("floor-hold resolved the row, want it still due")
	}
	// A held row still counts as attempted: Done must include the
	// skip, or watchers read a pass that never finishes its plan.
	if cur, _ := s.GetCurrent(testCtx()); cur.Done != 1 || cur.Due != 1 {
		t.Errorf("current = %+v, want Due 1 Done 1 (hold counts)", cur)
	}
}

// A registry 500 keeps the row due for next-tick retry and records
// the failure where `sweep`'s watch surfaces it. If this fails,
// failures are swallowed (row silently resolved) or crash the pass.
func TestRegistryFailureKeepsRowForRetry(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted,
		failFirst: true, err: errors.New("delete app/v1: registry status 500")}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}

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

// A trigger that finds the lock emits skip and stacks nothing: no
// deletes, no per-row outcomes, current stage reads skip. If this
// fails, concurrent passes delete together or a skip looks like work.
func TestLockedTriggerSkips(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
	held, _ := s.AcquireLock(testCtx(), store.LockKey, time.Minute)
	if !held {
		t.Fatal("could not pre-hold the lock")
	}
	defer func() { _ = s.ReleaseLock(testCtx(), store.LockKey) }()
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}

	sum := sw.RunPass(testCtx(), "POST")
	if !sum.Skipped {
		t.Errorf("summary = %+v, want Skipped", sum)
	}
	if n := len(stub.refs); n != 0 {
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
	s := store.NewMemStore()
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}

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
// recordSink captures emitted activity records so tests prove the
// sweeper narrates every pass — the same records Quickwit indexes.
type logRecord struct {
	msg   string
	attrs map[string]any
}

type recordSink struct {
	mu      sync.Mutex
	records []logRecord
}

func (r *recordSink) log(_ context.Context, msg string, args ...any) {
	rec := logRecord{msg: msg, attrs: map[string]any{}}
	for i := 0; i+1 < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			continue
		}
		rec.attrs[k] = args[i+1]
	}
	r.mu.Lock()
	r.records = append(r.records, rec)
	r.mu.Unlock()
}

func (r *recordSink) byMsg(msg string) []logRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []logRecord
	for _, rec := range r.records {
		if rec.msg == msg {
			out = append(out, rec)
		}
	}
	return out
}

// Every pass narrates itself to the activity log: one record per
// resolved row plus a pass summary. These records are what Quickwit
// indexes — if this fails, the console knows but observability is
// blind.
func TestPassEmitsActivityLogRecords(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	sink := &recordSink{}
	sw.Log = sink.log

	sum := sw.RunPass(testCtx(), "POST")

	rows := sink.byMsg("sweep row")
	if len(rows) != 1 {
		t.Fatalf("row records = %d, want 1", len(rows))
	}
	row := rows[0].attrs
	for k, want := range map[string]any{
		"repo": "app", "tag": "v1", "reason": "test", "outcome": "deleted",
	} {
		if row[k] != want {
			t.Errorf("row %s = %v, want %v", k, row[k], want)
		}
	}
	if row["pass_id"] != sum.PassID || sum.PassID == "" {
		t.Errorf("row pass_id = %v, want summary PassID %q", row["pass_id"], sum.PassID)
	}
	passes := sink.byMsg("sweep pass")
	if len(passes) != 1 {
		t.Fatalf("pass records = %d, want 1", len(passes))
	}
	pass := passes[0].attrs
	for k, want := range map[string]any{
		"trigger": "POST", "performed": 1, "planned": 0,
		"failed": 0, "untracked": 0, "skipped": false, "dry_run": false,
	} {
		if pass[k] != want {
			t.Errorf("pass %s = %v, want %v", k, pass[k], want)
		}
	}
	if pass["pass_id"] != sum.PassID {
		t.Errorf("pass pass_id = %v, want %q", pass["pass_id"], sum.PassID)
	}
	// A clean pass carries no failures key at all (not an empty
	// one): downstream log queries distinguish "none" from "empty".
	if _, ok := pass["failures"]; ok {
		t.Errorf("clean pass carries failures key: %v", pass)
	}
}

// A skipped pass still logs its summary (skipped=true, no rows): in
// Quickwit, ticks with nothing due must be distinguishable from a
// sweeper that stopped ticking.
func TestSkippedPassLogsSummary(t *testing.T) {
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	s := store.NewMemStore()
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	sink := &recordSink{}
	sw.Log = sink.log

	sum := sw.RunPass(testCtx(), "tick")
	if !sum.Skipped {
		t.Fatalf("summary = %+v, want skipped", sum)
	}
	if rows := sink.byMsg("sweep row"); len(rows) != 0 {
		t.Errorf("row records = %d, want 0 for a skipped pass", len(rows))
	}
	passes := sink.byMsg("sweep pass")
	if len(passes) != 1 || passes[0].attrs["skipped"] != true {
		t.Errorf("pass records = %+v, want one skipped summary", passes)
	}
}

// A failed row carries its error on the record: counts alone don't
// tell the operator why a digest delete 405d.
func TestFailedRowLogsError(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted,
		failFirst: true, err: errors.New("delete app/v1: registry status 500")}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	sink := &recordSink{}
	sw.Log = sink.log

	_ = sw.RunPass(testCtx(), "POST")

	rows := sink.byMsg("sweep row")
	if len(rows) != 1 || rows[0].attrs["outcome"] != "failed" {
		t.Fatalf("row records = %+v, want one failed outcome", rows)
	}
	errmsg, _ := rows[0].attrs["err"].(string)
	if !strings.Contains(errmsg, "500") {
		t.Errorf("row err = %q, want the registry status", errmsg)
	}
}

func TestRunPassDefaultsToWallClock(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	sw := &Sweeper{Store: s, Sentinel: pairGround(s), DryRun: true}
	if sum := sw.RunPass(testCtx(), "tick"); sum.Planned != 1 {
		t.Errorf("summary = %+v, want 1 planned on wall clock", sum)
	}
}

// errCurrentStore fails pass-record and activity writes (redis lost
// mid-pass) while keeping rows readable.
type errCurrentStore struct {
	*store.MemStore
}

func (errCurrentStore) SetCurrent(context.Context, store.Current) error {
	return errEventPath
}

func (errCurrentStore) PushActivity(context.Context, store.Outcome) error {
	return errEventPath
}

func (errCurrentStore) ReleaseLock(context.Context, string) error {
	return errEventPath
}

// errDeleteStore fails row deletes: the registry confirmed, but the
// resolution write did not land.
type errDeleteStore struct {
	*store.MemStore
}

func (errDeleteStore) Delete(context.Context, string, string) error {
	return errEventPath
}

type errEventPathT string

func (e errEventPathT) Error() string { return string(e) }

const errEventPath = errEventPathT("event path down")

// The event path degrades to logs, never to a failed pass: a redis
// blip mid-pass must not abort work or resolve rows wrongly, and the
// failure stays visible in logs. If this fails, store hiccups either
// fail passes spuriously or vanish silently.
func TestPassSurvivesEventWriteFailure(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	s := &errCurrentStore{MemStore: store.NewMemStore()}
	_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
	sw := &Sweeper{Store: s, Sentinel: pairGround(s), DryRun: true}
	if sum := sw.RunPass(testCtx(), "tick"); sum.Planned != 1 {
		t.Errorf("summary = %+v, want the pass to complete despite event-path errors", sum)
	}
	// Each failing write logs its own line: a shared substring check
	// would pass with only one of them logging.
	for _, want := range []string{"current write failed", "activity write failed", "lock release failed"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %q:\n%s", want, buf.String())
		}
	}
}

// A confirmed delete whose row resolution fails keeps the row for the
// next tick while still counting the performed registry delete: the
// registry did the work, only the bookkeeping missed. If this fails, a
// store hiccup either drops unconfirmed rows or undercounts the pass.
func TestConfirmedDeleteWithFailedResolution(t *testing.T) {
	for name, outcome := range map[string]string{
		"deleted":   registry.OutcomeDeleted,
		"untracked": registry.OutcomeHeld,
	} {
		s := &errDeleteStore{MemStore: store.NewMemStore()}
		_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
		stub := &stubRegistry{outcome: outcome}
		sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}

		var buf bytes.Buffer
		old := log.Writer()
		log.SetOutput(&buf)
		sum := sw.RunPass(testCtx(), "tick")
		log.SetOutput(old)

		if sum.Performed+sum.Untracked != 1 {
			t.Errorf("%s: summary = %+v, want the confirmed outcome counted", name, sum)
		}
		if all, _ := s.All(testCtx()); len(all) != 1 {
			t.Errorf("%s: row resolved without a confirmed write, want it kept", name)
		}
		if !strings.Contains(buf.String(), "row delete failed") {
			t.Errorf("%s: log missing the visible resolution failure", name)
		}
	}
}

// A delete that fails keeps its row due while the pass moves on: the
// first attempted row fails, the second still performs. The failed
// ref is whichever the stub recorded first — MemStore.Due ranges
// over a map, so the test must not assume row order. If this fails
// (deterministically, not flakily), the sweeper drops work it never
// confirmed or stops the pass at the first failure.
func TestSweeperFailedDeleteKeepsRowDue(t *testing.T) {
	s := store.NewMemStore()
	mkrow := func(tag, digest string) policy.Row {
		return policy.Row{Repo: "app", Tag: tag, Digest: digest,
			PushedAt: sweepNow.Add(-time.Hour), Due: true, Reason: "test"}
	}
	_ = s.Record(testCtx(), mkrow("v1", "sha256:aaa"))
	_ = s.Record(testCtx(), mkrow("v2", "sha256:bbb"))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted,
		failFirst: true, err: errors.New("registry down")}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	sink := &recordSink{}
	sw.Log = sink.log
	sum := sw.RunPass(testCtx(), "test")
	if sum.Performed != 1 || sum.Failed != 1 {
		t.Errorf("summary = %+v, want 1 performed and 1 failed", sum)
	}
	if len(stub.refs) != 2 {
		t.Fatalf("deleted refs = %v, want both rows attempted", stub.refs)
	}
	due, _ := s.Due(testCtx())
	if len(due) != 1 || due[0].Repo+"@"+due[0].Digest != stub.refs[0] {
		t.Errorf("due = %v, want only the first-attempted ref %q", due, stub.refs[0])
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, r := range sink.records {
		if r.msg != "sweep pass" {
			continue
		}
		fails, ok := r.attrs["failures"].([]string)
		if !ok || len(fails) == 0 {
			t.Errorf("pass log lacks failures: %v", r.attrs)
		}
	}
}

// A held manifest (owned by an index) untracks instead of retrying:
// the row leaves the store and the pass counts it untracked. If this
// fails, held rows either pile up due forever or count as performed.
func TestSweeperHeldDeleteUntracksRow(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	stub := &stubRegistry{outcome: registry.OutcomeHeld}
	sw := &Sweeper{Store: s, Registry: stub, Sentinel: pairGround(s), Now: func() time.Time { return sweepNow }}
	sum := sw.RunPass(testCtx(), "test")
	if sum.Untracked != 1 {
		t.Errorf("summary = %+v, want 1 untracked", sum)
	}
	if all, _ := s.All(testCtx()); len(all) != 0 {
		t.Errorf("%d rows survived a held delete, want 0", len(all))
	}
}
