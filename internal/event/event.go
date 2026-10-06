// Package event is the shared narration vocabulary: one Event
// shape, one Reporter port, one Emit, importing nothing. Run
// narration (gc to the operator's terminal) and flip narration
// (gate to serve's log) are separate rivers that never share a
// value — this package is the language they both speak. Stage
// strings stay local per side (matching strings is cosmetic,
// never logic).
package event

import (
	"time"
)

// Event is one lifecycle stage of a fenced run: gc collection,
// fence flips, both. The JSON tags keep it suitable for the
// JSON-lines transport the sweeper reports on, should the
// console ever subscribe. Stage strings are rendering labels —
// matching strings across packages is cosmetic, never logic,
// so gc and fencing voice their own stage consts and share
// only this shape.
type Event struct {
	Stage     string `json:"stage"`
	ElapsedMs int64  `json:"elapsed_ms"`
	PID       int    `json:"pid,omitempty"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Reporter receives lifecycle events. Nil reporters are fine:
// every emitter checks before delivering.
type Reporter func(Event)

// Timed stamps one lifecycle event against the run start.
func Timed(stage string, started time.Time) Event {
	return Event{Stage: stage, ElapsedMs: time.Since(started).Milliseconds()}
}

// Emit delivers one lifecycle event. Nil reporters are fine.
func Emit(report Reporter, event Event) {
	if report != nil {
		report(event)
	}
}
