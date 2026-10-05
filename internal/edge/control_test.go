package edge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Explicit transitions announce at the moment of the call: Deny
// voices deny_engage and Allow voices deny_release through both
// channels (report stage + ring outcome, signed kpr-edge) — loud
// with zero traffic. If this fails, lock/unlock flip the marker
// silently and the console learns only from pushes.
func TestControlAnnouncesTransitions(t *testing.T) {
	var events []gc.Event
	st := store.NewMemStore()
	c := Control{Store: st, Report: func(e gc.Event) { events = append(events, e) }}
	ctx := context.Background()
	c.Deny(ctx, "store locked for test")
	c.Allow(ctx, "store unlocked for test")
	if len(events) != 2 || events[0].Stage != StageDenyEngage || events[1].Stage != StageDenyRelease {
		t.Fatalf("events = %v, want deny_engage then deny_release", events)
	}
	if events[0].Message != "store locked for test" || events[1].Message != "store unlocked for test" {
		t.Errorf("events carry %q and %q, want the reasons given", events[0].Message, events[1].Message)
	}
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

// A storeless Control still voices the transition: the report
// channel never depends on the ring. If this fails, storeless
// transitions panic instead of announcing.
func TestControlAnnouncesWithoutStore(t *testing.T) {
	var events []gc.Event
	c := Control{Report: func(e gc.Event) { events = append(events, e) }}
	c.Deny(context.Background(), "locked for test")
	if len(events) != 1 || events[0].Stage != StageDenyEngage {
		t.Errorf("events = %v, want the deny_engage transition", events)
	}
}

// Hold voices its own transitions: hold_engage at engage time,
// hold_release on release — through both channels, like the
// deny transitions. A Hold that fails to engage voices
// nothing: no lease, no announcement. If this fails, an armed
// collect holds pushes silently on a quiet store.
func TestControlHoldVoicesTransitions(t *testing.T) {
	var events []gc.Event
	st := store.NewMemStore()
	c := Control{HoldFile: HoldFile{Dir: t.TempDir()}, Store: st, Report: func(e gc.Event) { events = append(events, e) }}
	ctx := context.Background()
	release, err := c.Hold(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if len(events) != 1 || events[0].Stage != StageHoldEngage {
		t.Fatalf("events = %v, want hold_engage at engage time", events)
	}
	release()
	if len(events) != 2 || events[1].Stage != StageHoldRelease {
		t.Fatalf("events = %v, want hold_release on release", events)
	}
	activity, err := st.Activity(ctx)
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	// The ring prepends: newest (release) first.
	if len(activity) != 2 || activity[1].Outcome != "hold_engage" || activity[0].Outcome != "hold_release" {
		t.Fatalf("ring = %v, want both hold transitions", activity)
	}
}

// A Hold that fails to engage voices nothing: the refusal
// carries the error, not an announcement. If this fails, a
// failed fence announces a lease that was never written.
func TestControlHoldFailureVoicesNothing(t *testing.T) {
	var events []gc.Event
	st := store.NewMemStore()
	c := Control{HoldFile: HoldFile{Dir: filepath.Join(t.TempDir(), "no-such-dir")}, Store: st, Report: func(e gc.Event) { events = append(events, e) }}
	if _, err := c.Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("Hold into a missing dir succeeded, want refusal")
	}
	if len(events) != 0 {
		t.Errorf("events = %v, want silence on failed engage", events)
	}
	activity, err := st.Activity(context.Background())
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if len(activity) != 0 {
		t.Errorf("ring = %v, want silence on failed engage", activity)
	}
}
