// Package storetest holds the shared Store contract: one body of
// scenarios every backend must satisfy. The unit suite runs it
// against the mem store; the e2e suite runs it against
// testcontainers redis (auth, kpr's DB) — one contract, never a copy
// per backend.
package storetest

import (
	"context"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// RunContract executes every contract scenario with a store from
// setup — mem hands a fresh one per subtest, redis flushes a shared
// one. Backend failures fatal (broken store); verdicts live in the
// scenarios.
func RunContract(t *testing.T, setup func(t *testing.T) store.Store) {
	t.Helper()
	t.Run("record", func(t *testing.T) { testRecordAndAll(t, setup(t)) })
	t.Run("mark", func(t *testing.T) { testMarkDuePersists(t, setup(t)) })
	t.Run("mark-creates", func(t *testing.T) { testMarkDueCreatesRow(t, setup(t)) })
	t.Run("clear", func(t *testing.T) { testClearDueEmptiesMarks(t, setup(t)) })
	t.Run("unmark", func(t *testing.T) { testUnmarkDueClearsOneMark(t, setup(t)) })
	t.Run("repush", func(t *testing.T) { testRecordRepushClearsStaleMark(t, setup(t)) })
	t.Run("delete", func(t *testing.T) { testDeleteRemovesRow(t, setup(t)) })
	t.Run("current", func(t *testing.T) { testCurrentRoundTrip(t, setup(t)) })
	t.Run("activity", func(t *testing.T) { testActivityRingCapped(t, setup(t)) })
	t.Run("lock", func(t *testing.T) { testLockSingleFlight(t, setup(t)) })
	t.Run("gc-lock", func(t *testing.T) { testGCLockSingleFlight(t, setup(t)) })
}

var storeNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func srow(repo, tag string) policy.Row {
	return policy.Row{Repo: repo, Tag: tag, Digest: "sha256:abc", PushedAt: storeNow}
}

func ctx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = cancel
	return c
}

// Every backend must round-trip recorded rows: the receiver writes what
// the console and reap later read. If this fails, pushes vanish between
// record and plan and the whole pipeline reasons about nothing.
func testRecordAndAll(t *testing.T, s store.Store) {
	c := ctx()
	if err := s.Ping(c); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := s.Record(c, srow("app", "v1")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Record(c, srow("app", "v2")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := s.All(c)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("All = %d rows, want 2", len(got))
	}
}

// A reap mark (due + reason) must survive on the row until the sweeper
// resolves it, and Due must return only marked rows. If this fails,
// marks evaporate (sweep never fires) or unmarked rows look due (sweep
// deletes what nobody approved).
func testMarkDuePersists(t *testing.T, s store.Store) {
	c := ctx()
	_ = s.Record(c, srow("app", "v1"))
	_ = s.Record(c, srow("app", "v2"))
	if err := s.MarkDue(c, "app", "v1", "ttl:10m elapsed"); err != nil {
		t.Fatalf("MarkDue: %v", err)
	}
	due, err := s.Due(c)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 1 || due[0].Tag != "v1" || !due[0].Due {
		t.Fatalf("Due = %+v, want [v1 marked]", due)
	}
	if due[0].Reason != "ttl:10m elapsed" {
		t.Errorf("Reason = %q, want the mark reap wrote", due[0].Reason)
	}
}

// A re-push (newer push time) restarts the minimal promise, so it must
// clear a stale due mark — otherwise the sweeper deletes a fresh push
// on reasoning from before it existed. A re-notification of the same
// push preserves the mark. If this fails, redeploys get swept.
func testRecordRepushClearsStaleMark(t *testing.T, s store.Store) {
	c := ctx()
	_ = s.Record(c, srow("app", "10m"))
	_ = s.MarkDue(c, "app", "10m", "ttl:10m elapsed")

	same := srow("app", "10m")
	_ = s.Record(c, same)
	if due, _ := s.Due(c); len(due) != 1 {
		t.Fatalf("same-push re-record cleared the mark, want preserved")
	}

	fresh := srow("app", "10m")
	fresh.PushedAt = storeNow.Add(time.Hour)
	_ = s.Record(c, fresh)
	if due, _ := s.Due(c); len(due) != 0 {
		t.Fatalf("re-push kept %d due rows, want 0 (promise restarted)", len(due))
	}
}

// Marking a never-recorded row creates it (skeletal, then filled by
// the next notification): reap reasons about tags the receiver hasn't
// seen yet, and the mark must land somewhere. If this fails, marks on
// unseen rows vanish instead of awaiting the push.
func testMarkDueCreatesRow(t *testing.T, s store.Store) {
	c := ctx()
	if err := s.MarkDue(c, "new", "v9", "keep-n:exceeds 10"); err != nil {
		t.Fatalf("MarkDue: %v", err)
	}
	due, err := s.Due(c)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 1 || due[0].Repo != "new" || due[0].Tag != "v9" {
		t.Fatalf("Due = %+v, want the created mark", due)
	}
	if !due[0].Due || due[0].Reason != "keep-n:exceeds 10" {
		t.Errorf("created row not marked: %+v", due[0])
	}
}

// Discarding the plan drops every due mark and reports the count while
// rows survive; a second clear is a zero no-op. If this fails, pardon
// either keeps rows marked (sweep eats them anyway) or deletes rows.
func testClearDueEmptiesMarks(t *testing.T, s store.Store) {
	c := ctx()
	_ = s.Record(c, srow("app", "v1"))
	_ = s.Record(c, srow("app", "v2"))
	_ = s.MarkDue(c, "app", "v1", "keep-n:exceeds 10")
	_ = s.MarkDue(c, "app", "v2", "ttl:10m elapsed")
	n, err := s.ClearDue(c)
	if err != nil {
		t.Fatalf("ClearDue: %v", err)
	}
	if n != 2 {
		t.Errorf("ClearDue = %d, want 2", n)
	}
	if due, _ := s.Due(c); len(due) != 0 {
		t.Fatalf("%d marks survived, want 0", len(due))
	}
	if all, _ := s.All(c); len(all) != 2 {
		t.Errorf("All = %d rows after clear, want 2 surviving rows", len(all))
	}
	if n, _ := s.ClearDue(c); n != 0 {
		t.Errorf("second ClearDue = %d, want 0", n)
	}
}

// Unmarking drops one due mark and reports it; other marks, rows,
// and non-marks are untouched. If this fails, plan remove either
// clears the world (ClearDue) or lies about what went.
func testUnmarkDueClearsOneMark(t *testing.T, s store.Store) {
	c := ctx()
	_ = s.Record(c, srow("app", "v1"))
	_ = s.Record(c, srow("app", "v2"))
	_ = s.MarkDue(c, "app", "v1", "manual")
	_ = s.MarkDue(c, "app", "v2", "manual")
	ok, err := s.UnmarkDue(c, "app", "v1")
	if err != nil {
		t.Fatalf("UnmarkDue: %v", err)
	}
	if !ok {
		t.Error("UnmarkDue(app:v1) = false, want true for a held mark")
	}
	if due, _ := s.Due(c); len(due) != 1 || due[0].Tag != "v2" {
		t.Errorf("Due after unmark = %v, want only app:v2", due)
	}
	if all, _ := s.All(c); len(all) != 2 {
		t.Errorf("All = %d rows after unmark, want 2 surviving rows", len(all))
	}
	if ok, _ := s.UnmarkDue(c, "app", "v1"); ok {
		t.Error("second UnmarkDue(app:v1) = true, want false (mark gone)")
	}
	if ok, _ := s.UnmarkDue(c, "app", "v9"); ok {
		t.Error("UnmarkDue(app:v9) = true, want false (never marked)")
	}
}

// A confirmed registry delete removes the row: the sweeper resolves
// marks by deleting, not by unmarking. If this fails, swept rows haunt
// every future plan.
func testDeleteRemovesRow(t *testing.T, s store.Store) {
	c := ctx()
	_ = s.Record(c, srow("app", "v1"))
	if err := s.Delete(c, "app", "v1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	all, err := s.All(c)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("All = %d rows after delete, want 0", len(all))
	}
}

// Run state round-trips the current pass record so a CLI can report
// what the sweeper is doing right now without streaming logs. If this
// fails, `sweep` watches a stale or empty pass.
func testCurrentRoundTrip(t *testing.T, s store.Store) {
	c := ctx()
	want := store.Current{PassID: "p1", Stage: "running", Trigger: "POST", Due: 3, Done: 1}
	if err := s.SetCurrent(c, want); err != nil {
		t.Fatalf("SetCurrent: %v", err)
	}
	got, err := s.GetCurrent(c)
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	if got != want {
		t.Errorf("GetCurrent = %+v, want %+v", got, want)
	}
}

// The activity ring is bounded state, not a log stream: past the cap
// the oldest outcomes drop, newest first on read. If this fails, redis
// grows a log feed — exactly what the plan forbids.
func testActivityRingCapped(t *testing.T, s store.Store) {
	c := ctx()
	for i := 0; i < store.ActivityCap+5; i++ {
		if err := s.PushActivity(c, store.Outcome{Repo: "app", Reason: "x"}); err != nil {
			t.Fatalf("PushActivity: %v", err)
		}
	}
	got, err := s.Activity(c)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(got) != store.ActivityCap {
		t.Errorf("Activity = %d outcomes, want cap %d", len(got), store.ActivityCap)
	}
}

// The collector lock is single-flight like the sweep lock but on its
// own key: a second collector refuses while one runs, and releasing
// re-arms. If this fails, gc runs race each other on one store.
func testGCLockSingleFlight(t *testing.T, s store.Store) {
	c := ctx()
	ok, err := s.AcquireGCLock(c, time.Minute)
	if err != nil || !ok {
		t.Fatalf("first AcquireGCLock = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = s.AcquireGCLock(c, time.Minute)
	if err != nil || ok {
		t.Fatalf("second AcquireGCLock = (%v, %v), want (false, nil)", ok, err)
	}
	if err := s.ReleaseGCLock(c); err != nil {
		t.Fatalf("ReleaseGCLock: %v", err)
	}
	ok, err = s.AcquireGCLock(c, time.Minute)
	if err != nil || !ok {
		t.Fatalf("post-release AcquireGCLock = (%v, %v), want (true, nil)", ok, err)
	}
	_ = s.ReleaseGCLock(c)
}

// The sweep lock is single-flight with expiry: a second trigger skips
// instead of stacking, and release re-arms. If this fails, two passes
// delete concurrently or a crashed sweeper holds the lock forever.
func testLockSingleFlight(t *testing.T, s store.Store) {
	c := ctx()
	ok, err := s.AcquireLock(c, time.Minute)
	if err != nil || !ok {
		t.Fatalf("first AcquireLock = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = s.AcquireLock(c, time.Minute)
	if err != nil || ok {
		t.Fatalf("second AcquireLock = (%v, %v), want (false, nil)", ok, err)
	}
	if err := s.ReleaseLock(c); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}
	ok, err = s.AcquireLock(c, time.Minute)
	if err != nil || !ok {
		t.Fatalf("post-release AcquireLock = (%v, %v), want (true, nil)", ok, err)
	}
	_ = s.ReleaseLock(c)
}
