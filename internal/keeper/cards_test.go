package keeper_test

import (
	"context"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// The store card's standing is decided at assembly, never at
// render: unlocked reads ok, locked reads locked, a lock read
// failure reads degraded, a failed probe unreachable, no store
// unavailable. If this fails, both renderers voice the wrong
// state from the same facts.
func TestStoreCardStandingMatrix(t *testing.T) {
	ctx := context.Background()
	open := store.NewMemStore()
	if err := open.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	locked := store.NewMemStore()
	for _, tc := range []struct {
		name    string
		st      store.Store
		healthy bool
		proven  bool
		want    keeper.Standing
		note    string
	}{
		{"open mem", open, true, false, keeper.StandingOK, ""},
		{"locked mem", locked, true, false, keeper.StandingLocked, "locked"},
		{"down mem", open, false, false, keeper.StandingUnreachable, ""},
		{"locked and down", locked, false, false, keeper.StandingLocked, "locked"},
		{"no store", nil, false, false, keeper.StandingUnavailable, ""},
	} {
		got := keeper.FetchStoreCard(ctx, tc.st, tc.healthy, "", 0, tc.proven)
		if got.Standing != tc.want || got.LockNote != tc.note {
			t.Errorf("%s = (%q, note %q), want (%q, note %q)",
				tc.name, got.Standing, got.LockNote, tc.want, tc.note)
		}
	}
}

// A backend that answers nothing degrades the card instead of
// guessing intent: "lock unknown", never collapsed into locked
// or unlocked. If this fails, a down backend reads as an
// operator decision.
func TestStoreCardLockFailureDegrades(t *testing.T) {
	rdb := store.NewRedisStore("127.0.0.1:1", "", 0)
	defer func() { _ = rdb.Close() }()
	got := keeper.FetchStoreCard(context.Background(), rdb, false, "127.0.0.1:1", 0, false)
	if got.Standing != keeper.StandingDegraded || got.LockNote != "lock unknown" {
		t.Errorf("down redis = (%q, note %q), want (degraded, lock unknown)", got.Standing, got.LockNote)
	}
}

// The card names the wired backend: mem voices itself, file
// voices its dir, redis its address and DB. If this fails, the
// card misnames the backend serve actually opened.
func TestStoreCardNamesBackend(t *testing.T) {
	ctx := context.Background()
	if got := keeper.FetchStoreCard(ctx, store.NewMemStore(), true, "", 0, false); got.Name != "mem" {
		t.Errorf("mem card names %q", got.Name)
	}
	dir := t.TempDir()
	if got := keeper.FetchStoreCard(ctx, store.NewFileStore(dir), true, "", 0, false); got.Name != "file" || got.Detail != dir {
		t.Errorf("file card = (%q, %q), want (file, %q)", got.Name, got.Detail, dir)
	}
	rdb := store.NewRedisStore("127.0.0.1:1", "", 3)
	defer func() { _ = rdb.Close() }()
	if got := keeper.FetchStoreCard(ctx, rdb, true, "127.0.0.1:1", 3, false); got.Name != "redis" || got.Detail != "127.0.0.1:1 db 3" {
		t.Errorf("redis card = (%q, %q)", got.Name, got.Detail)
	}
}

// Filesystem size voices only on a proven store: an unproven
// dir is nobody's store to report on. If this fails, foreign
// dirs get capacity ink.
func TestStoreCardFsNoteGatedOnProof(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	if got := keeper.FetchStoreCard(ctx, s, true, "", 0, false); got.FsNote != "" {
		t.Errorf("unproven FsNote = %q, want silence", got.FsNote)
	}
	if got := keeper.FetchStoreCard(ctx, s, true, "", 0, true); got.FsNote == "" {
		t.Error("proven FsNote empty, want capacity")
	}
	if got := keeper.FetchStoreCard(ctx, nil, false, "", 0, true); got.Name != "unavailable" || got.FsNote != "" {
		t.Errorf("storeless card = (%q, %q), want (unavailable, silence)", got.Name, got.FsNote)
	}
}
