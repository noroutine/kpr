package event

import (
	"testing"
	"time"
)

// Nil reporters are fine: Emit skips them instead of panicking.
// If this fails, an unconfigured run crashes on its first event.
func TestEmitSkipsNilReporter(t *testing.T) {
	Emit(nil, Event{Stage: "sweep"})
}

// Emit delivers the event as-is to a set reporter: the reporter
// sees the same value, never a copy with the fields rearranged.
// If this fails, narration drops or rewrites lifecycle events.
func TestEmitDeliversToReporter(t *testing.T) {
	var got []Event
	Emit(func(e Event) { got = append(got, e) }, Event{Stage: "sweep", ElapsedMs: 7})
	if len(got) != 1 || got[0].Stage != "sweep" || got[0].ElapsedMs != 7 {
		t.Errorf("delivered = %+v, want the one event untouched", got)
	}
}

// Timed stamps the stage against the run start: the stage voices
// itself and the elapsed reads zero for a just-started run. If
// this fails, run narration mislabels or misdates its events.
func TestTimedStampsStageAndElapsed(t *testing.T) {
	got := Timed("sweep", time.Now())
	if got.Stage != "sweep" || got.ElapsedMs != 0 {
		t.Errorf("timed = %+v, want stage sweep with zero elapsed", got)
	}
}
