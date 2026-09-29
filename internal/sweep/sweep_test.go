package sweep

import (
	"bytes"
	"context"
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

func (f *fakeRegistry) lastRef() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refs) == 0 {
		return ""
	}
	return f.refs[len(f.refs)-1]
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

// stubRegistry answers DeleteManifest without HTTP: the sweeper must
// consume the registry through a small port, not a concrete client.
// If this fails, the sweep use case is still coupled to the transport.
type stubRegistry struct {
	mu      sync.Mutex
	refs    []string
	outcome string
	err     error
}

func (f *stubRegistry) DeleteManifest(ctx context.Context, repo, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, repo+"@"+ref)
	return f.outcome, f.err
}

// An armed pass deletes through the Registry port: the stub records
// the digest reference and the confirmed row leaves the store. If
// this fails, the sweep use case bypasses its port or deletes by tag
// instead of digest.
func TestSweeperDeletesViaStubRegistry(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	stub := &stubRegistry{outcome: registry.OutcomeDeleted}
	sw := &Sweeper{Store: s, Registry: stub, Now: func() time.Time { return sweepNow }}
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
	f := newFake(http.StatusAccepted, "")
	defer f.close()
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
	sw := newSweeper(s, f.srv.URL, false)

	if sum := sw.RunPass(testCtx(), "tick"); sum.Performed != 1 {
		t.Fatalf("summary = %+v, want Performed 1", sum)
	}
	if ref := f.lastRef(); !strings.HasSuffix(ref, "sha256:abc") {
		t.Errorf("deleted ref = %q, want the row digest", ref)
	}

	tagOnly := duerow("app", "v9", 200*24*time.Hour)
	tagOnly.Digest = ""
	_ = s.Record(testCtx(), tagOnly)
	if sum := sw.RunPass(testCtx(), "tick"); sum.Performed != 1 {
		t.Fatalf("summary = %+v, want the digest-less row attempted too", sum)
	}
	if ref := f.lastRef(); !strings.HasSuffix(ref, "v9") {
		t.Errorf("deleted ref = %q, want tag fallback for digest-less rows", ref)
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
	if cur, _ := s.GetCurrent(testCtx()); cur.Done != 1 || cur.Due != 1 {
		t.Errorf("current = %+v, want Due 1 Done 1", cur)
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
	f := newFake(http.StatusAccepted, "")
	defer f.close()
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	sw := newSweeper(s, f.srv.URL, false)
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
}

// A skipped pass still logs its summary (skipped=true, no rows): in
// Quickwit, ticks with nothing due must be distinguishable from a
// sweeper that stopped ticking.
func TestSkippedPassLogsSummary(t *testing.T) {
	f := newFake(http.StatusAccepted, "")
	defer f.close()
	sw := newSweeper(store.NewMemStore(), f.srv.URL, false)
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
	f := newFake(http.StatusInternalServerError, "boom")
	defer f.close()
	s := store.NewMemStore()
	_ = s.Record(testCtx(), duerow("app", "v1", time.Hour))
	sw := newSweeper(s, f.srv.URL, false)
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
	sw := &Sweeper{Store: s, DryRun: true}
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

func (errCurrentStore) ReleaseLock(context.Context) error {
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
	sw := &Sweeper{Store: s, DryRun: true}
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
	for name, status := range map[string]struct {
		code int
		body string
	}{
		"deleted":   {http.StatusAccepted, ""},
		"untracked": {http.StatusMethodNotAllowed, `{"errors":[{"code":"DENIED"}]}`},
	} {
		f := newFake(status.code, status.body)
		defer f.close()
		s := &errDeleteStore{MemStore: store.NewMemStore()}
		_ = s.Record(testCtx(), duerow("app", "v1", 200*24*time.Hour))
		sw := newSweeper(s, f.srv.URL, false)

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
