package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/testing/storetest"
)

// The in-memory backend implements the full contract so the sweeper,
// CLI, and console tests never need a live redis. The redis leg of
// the same contract runs in e2e (test/e2e, fixture redis with auth on
// kpr's DB). If this fails, nothing above it can be tested
// hermetically.
func TestMemStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) store.Store { return store.NewMemStore() })
}

// Every redis command against a dead server must surface its error —
// the degraded path serve and the CLI rely on. A port nothing answers
// exercises all of them hermetically (connection refused is instant).
// If this fails, a redis outage panics or hangs instead of degrading.
func TestRedisStoreDeadServer(t *testing.T) {
	s := store.NewRedisStore("127.0.0.1:1", "", 0)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	row := policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:abc", PushedAt: time.Now()}

	if err := s.Ping(ctx); err == nil {
		t.Error("Ping = nil, want connection error")
	}
	if err := s.Flush(ctx); err == nil {
		t.Error("Flush = nil, want connection error")
	}
	if err := s.Record(ctx, row); err == nil {
		t.Error("Record = nil, want connection error")
	}
	if _, err := s.All(ctx); err == nil {
		t.Error("All = nil, want connection error")
	}
	if _, err := s.Due(ctx); err == nil {
		t.Error("Due = nil, want connection error")
	}
	if err := s.MarkDue(ctx, row.Repo, row.Tag, "x"); err == nil {
		t.Error("MarkDue = nil, want connection error")
	}
	if _, err := s.UnmarkDue(ctx, row.Repo, row.Tag); err == nil {
		t.Error("UnmarkDue = nil, want connection error")
	}
	if err := s.Delete(ctx, row.Repo, row.Tag); err == nil {
		t.Error("Delete = nil, want connection error")
	}
	if err := s.SetCurrent(ctx, store.Current{PassID: "p1"}); err == nil {
		t.Error("SetCurrent = nil, want connection error")
	}
	if _, err := s.GetCurrent(ctx); err == nil {
		t.Error("GetCurrent = nil, want connection error")
	}
	if err := s.PushActivity(ctx, store.Outcome{Repo: "app", Reason: "x"}); err == nil {
		t.Error("PushActivity = nil, want connection error")
	}
	if _, err := s.Activity(ctx); err == nil {
		t.Error("Activity = nil, want connection error")
	}
	if ok, err := s.AcquireLock(ctx, store.LockKey, time.Minute); err == nil || ok {
		t.Errorf("AcquireLock = %v/%v, want false with connection error", ok, err)
	}
	if err := s.ReleaseLock(ctx, store.LockKey); err == nil {
		t.Error("ReleaseLock = nil, want connection error")
	}
}

// A dead-server MarkDue must surface the connection failure itself,
// not a follow-on decoding error: the degraded path logs this error
// to explain the outage, and a JSON complaint would misdirect. If
// this fails, the HGet error is masked before it reaches the caller.
func TestRedisStoreDeadServerSurfacesRootError(t *testing.T) {
	s := store.NewRedisStore("127.0.0.1:1", "", 0)
	t.Cleanup(func() { _ = s.Close() })
	err := s.MarkDue(context.Background(), "app", "v1", "x")
	if err == nil {
		t.Fatal("MarkDue = nil, want connection error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refus") {
		t.Errorf("MarkDue error = %v, want the refused connection (not a follow-on)", err)
	}
}
