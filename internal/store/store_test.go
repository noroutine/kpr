package store

import (
	"context"
	"os"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
)

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
func testRecordAndAll(t *testing.T, s Store) {
	c := ctx()
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
func testMarkDuePersists(t *testing.T, s Store) {
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
func testRecordRepushClearsStaleMark(t *testing.T, s Store) {
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

// A confirmed registry delete removes the row: the sweeper resolves
// marks by deleting, not by unmarking. If this fails, swept rows haunt
// every future plan.
func testDeleteRemovesRow(t *testing.T, s Store) {
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
func testCurrentRoundTrip(t *testing.T, s Store) {
	c := ctx()
	want := Current{PassID: "p1", Stage: "running", Trigger: "POST", Due: 3, Done: 1}
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
func testActivityRingCapped(t *testing.T, s Store) {
	c := ctx()
	for i := 0; i < ActivityCap+5; i++ {
		if err := s.PushActivity(c, Outcome{Repo: "app", Reason: "x"}); err != nil {
			t.Fatalf("PushActivity: %v", err)
		}
	}
	got, err := s.Activity(c)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(got) != ActivityCap {
		t.Errorf("Activity = %d outcomes, want cap %d", len(got), ActivityCap)
	}
}

// The sweep lock is single-flight with expiry: a second trigger skips
// instead of stacking, and release re-arms. If this fails, two passes
// delete concurrently or a crashed sweeper holds the lock forever.
func testLockSingleFlight(t *testing.T, s Store) {
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

// The in-memory backend implements the full contract so the sweeper,
// CLI, and console tests never need a live redis. If this fails,
// nothing above it can be tested hermetically.
func TestMemStoreContract(t *testing.T) {
	fresh := func() Store { return NewMemStore() }
	t.Run("record", func(t *testing.T) { testRecordAndAll(t, fresh()) })
	t.Run("mark", func(t *testing.T) { testMarkDuePersists(t, fresh()) })
	t.Run("repush", func(t *testing.T) { testRecordRepushClearsStaleMark(t, fresh()) })
	t.Run("delete", func(t *testing.T) { testDeleteRemovesRow(t, fresh()) })
	t.Run("current", func(t *testing.T) { testCurrentRoundTrip(t, fresh()) })
	t.Run("activity", func(t *testing.T) { testActivityRingCapped(t, fresh()) })
	t.Run("lock", func(t *testing.T) { testLockSingleFlight(t, fresh()) })
}

// The redis backend implements the same contract against the real
// thing. It skips when no redis answers (unit suite stays green
// without fixtures); the address comes from the environment, never a
// hardcoded localhost (CI fixtures are siblings, not loopback).
func TestRedisStoreContract(t *testing.T) {
	addr := os.Getenv("KPR_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	s, err := NewRedisStore(addr)
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	if err := s.Ping(ctx()); err != nil {
		t.Skipf("redis at %s unreachable, skipping: %v", addr, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	flush := func(t *testing.T) {
		t.Helper()
		if err := s.Flush(ctx()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	t.Run("record", func(t *testing.T) { flush(t); testRecordAndAll(t, s) })
	t.Run("mark", func(t *testing.T) { flush(t); testMarkDuePersists(t, s) })
	t.Run("repush", func(t *testing.T) { flush(t); testRecordRepushClearsStaleMark(t, s) })
	t.Run("delete", func(t *testing.T) { flush(t); testDeleteRemovesRow(t, s) })
	t.Run("current", func(t *testing.T) { flush(t); testCurrentRoundTrip(t, s) })
	t.Run("activity", func(t *testing.T) { flush(t); testActivityRingCapped(t, s) })
	t.Run("lock", func(t *testing.T) { flush(t); testLockSingleFlight(t, s) })
}
