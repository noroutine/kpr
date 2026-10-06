package edge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// Explicit transitions record at the moment of the call: Deny
// records deny_engage and Allow records deny_release to the ring
// (signed kpr-edge) — loud with zero traffic. Ephemeral narration
// belongs to the caller. If this fails, lock/unlock flip the
// marker silently and the console learns only from pushes.
func TestControlRecordsTransitions(t *testing.T) {
	st := store.NewMemStore()
	c := Control{Store: st}
	ctx := context.Background()
	c.Deny(ctx, "store locked for test")
	c.Allow(ctx, "store unlocked for test")
	activity, err := st.Activity(ctx)
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	// The ring prepends: newest (release) first.
	if len(activity) != 2 || activity[1].Outcome != "deny_engage" || activity[0].Outcome != "deny_release" {
		t.Fatalf("ring = %v, want both transition outcomes", activity)
	}
	for i, want := range []string{"store unlocked for test", "store locked for test"} {
		if activity[i].Reason != want || activity[i].Actor != "kpr-edge" {
			t.Errorf("ring[%d] = %+v, want reason %q signed kpr-edge", i, activity[i], want)
		}
	}
}

// A storeless Control drops the transition silently: with no
// ring and no reporter there is nowhere to record. If this
// fails, storeless transitions panic instead of skipping.
func TestControlStorelessDenySkipsSilently(t *testing.T) {
	c := Control{}
	c.Deny(context.Background(), "locked for test")
}

// Hold records its own transitions: hold_engage at engage time,
// hold_release on release — to the ring, like the deny
// transitions. A Hold that fails to engage records
// nothing: no lease, no record. If this fails, an armed
// collect holds pushes silently on a quiet store.
func TestControlHoldRecordsTransitions(t *testing.T) {
	st := store.NewMemStore()
	c := Control{HoldFile: store.HoldFile{Dir: t.TempDir()}, Store: st}
	ctx := context.Background()
	release, err := c.Hold(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	release()
	activity, err := st.Activity(ctx)
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	// The ring prepends: newest (release) first.
	if len(activity) != 2 || activity[1].Outcome != "hold_engage" || activity[0].Outcome != "hold_release" {
		t.Fatalf("ring = %v, want both hold transitions", activity)
	}
}

// A Hold that fails to engage records nothing: the refusal
// carries the error, not a record. If this fails, a
// failed fence records a lease that was never written.
func TestControlHoldFailureRecordsNothing(t *testing.T) {
	st := store.NewMemStore()
	c := Control{HoldFile: store.HoldFile{Dir: filepath.Join(t.TempDir(), "no-such-dir")}, Store: st}
	if _, err := c.Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("Hold into a missing dir succeeded, want refusal")
	}
	activity, err := st.Activity(context.Background())
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if len(activity) != 0 {
		t.Errorf("ring = %v, want silence on failed engage", activity)
	}
}
