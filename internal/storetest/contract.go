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
	t.Run("lock", func(t *testing.T) { testNamedLockSingleFlight(t, setup(t)) })
	t.Run("unlock-marker", func(t *testing.T) { testUnlockMarkerFreshLocked(t, setup(t)) })
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

// Named locks are single-flight per key with expiry: a second holder
// of the same lock refuses while one runs, releasing re-arms, and one
// lock never blocks another. If this fails, two passes (or two
// collectors) race one store, one run wedges the rest, or a sweep
// deadlocks a collection it never touches.
func testNamedLockSingleFlight(t *testing.T, s store.Store) {
	c := ctx()
	for _, name := range []string{store.LockKey, store.GCLockKey} {
		ok, err := s.AcquireLock(c, name, time.Minute)
		if err != nil || !ok {
			t.Fatalf("%s: first acquire = (%v, %v), want (true, nil)", name, ok, err)
		}
		ok, err = s.AcquireLock(c, name, time.Minute)
		if err != nil || ok {
			t.Fatalf("%s: second acquire = (%v, %v), want (false, nil)", name, ok, err)
		}
		if err := s.ReleaseLock(c, name); err != nil {
			t.Fatalf("%s: release: %v", name, err)
		}
		ok, err = s.AcquireLock(c, name, time.Minute)
		if err != nil || !ok {
			t.Fatalf("%s: post-release acquire = (%v, %v), want (true, nil)", name, ok, err)
		}
		_ = s.ReleaseLock(c, name)
	}
	ok, err := s.AcquireLock(c, store.LockKey, time.Minute)
	if err != nil || !ok {
		t.Fatalf("sweep acquire = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := s.AcquireLock(c, store.GCLockKey, time.Minute); err != nil || !ok {
		t.Fatalf("gc acquire under held sweep lock = (%v, %v), want (true, nil)", ok, err)
	}
	_ = s.ReleaseLock(c, store.LockKey)
	_ = s.ReleaseLock(c, store.GCLockKey)
}

// A fresh store reads locked: registry-store writes stay denied
// until `kpr unlock` proves the shared store and records intent.
// Lock clears back to denied; double-lock stays quiet. If this
// fails, a fresh deploy collects on first gc, or intent doesn't
// survive the backend round-trip.
func testUnlockMarkerFreshLocked(t *testing.T, s store.Store) {
	c := ctx()
	if ok, err := s.IsUnlocked(c); err != nil || ok {
		t.Fatalf("fresh IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
	if err := s.SetUnlocked(c, true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if ok, err := s.IsUnlocked(c); err != nil || !ok {
		t.Fatalf("post-unlock IsUnlocked = (%v, %v), want (true, nil)", ok, err)
	}
	if err := s.SetUnlocked(c, false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if ok, err := s.IsUnlocked(c); err != nil || ok {
		t.Fatalf("post-lock IsUnlocked = (%v, %v), want (false, nil)", ok, err)
	}
	if err := s.SetUnlocked(c, false); err != nil {
		t.Fatalf("double lock: %v", err)
	}
}
