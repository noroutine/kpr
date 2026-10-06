package gc

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/fence"
)

type stubFencer struct {
	events  *[]string
	holdErr error
}

func (s stubFencer) Hold(context.Context, time.Time) (func(), error) {
	*s.events = append(*s.events, "hold")
	if s.holdErr != nil {
		return nil, s.holdErr
	}
	return func() { *s.events = append(*s.events, "release") }, nil
}

// Deny/Allow never fire on the gc path (only Hold does); the
// no-ops satisfy the port so the Hold engagement tests can run.
// If this fails, the stub stopped implementing the fence port.
func (s stubFencer) Deny(context.Context, string) {}

func (s stubFencer) Allow(context.Context, string) {}

func collectWithEvents(events *[]string) Collector {
	return func(_ context.Context, _ io.Writer, _ string, _ []string, _ fence.Reporter) error {
		*events = append(*events, "collect")
		return nil
	}
}

var errFenceBoom = errors.New("boom")

// An armed collect engages the fence around collection and
// releases after: hold, collect, release, in that order. If this
// fails, armed collects stopped holding the edge.
func TestArmedCollectHoldsFenceAroundCollect(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var events []string
	var out strings.Builder
	stageConfig(t, "http://registry:5000", cfg)
	err := Run(context.Background(), &out, Deps{Lock: s, Rec: s, Ids: s, Rows: s, API: fileAPI{root}, Clock: stubClock{}, Report: func(fence.Event) {}, Fence: stubFencer{events: &events}, Probe: probe, Collect: collectWithEvents(&events)},
		Options{DryRun: false}, Accepts{})
	if err != nil {
		t.Fatalf("stub-port run: %v", err)
	}
	want := []string{"hold", "collect", "release"}
	if len(events) != len(want) {
		t.Fatalf("fence order = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("fence order = %v, want %v", events, want)
		}
	}
}

// A preview never engages the fence: nothing is deleted, so
// nothing holds. If this fails, dry-runs started fencing pushes
// for no reason.
func TestPreviewNeverHoldsFence(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	stagePairedGen(t, s, root)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var events []string
	var out strings.Builder
	stageConfig(t, "http://registry:5000", cfg)
	err := Run(context.Background(), &out, Deps{Lock: s, Rec: s, Ids: s, Rows: s, API: fileAPI{root}, Clock: stubClock{}, Report: func(fence.Event) {}, Probe: probe, Collect: collectWithEvents(&events)},
		Options{DryRun: true}, Accepts{})
	if err != nil {
		t.Fatalf("stub-port run: %v", err)
	}
	if len(events) != 1 || events[0] != "collect" {
		t.Fatalf("preview events = %v, want exactly the bare collect", events)
	}
}

// A fence that fails to engage refuses the armed run: collecting
// unfenced when fencing was requested is unknown safety. If this
// fails, broken fences started collecting anyway.
func TestArmedCollectRefusesWhenFenceFails(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var events []string
	var out strings.Builder
	stageConfig(t, "http://registry:5000", cfg)
	err := Run(context.Background(), &out, Deps{Lock: s, Rec: s, Ids: s, Rows: s, API: fileAPI{root}, Clock: stubClock{}, Report: func(fence.Event) {}, Fence: stubFencer{events: &events, holdErr: errFenceBoom}, Probe: probe, Collect: collectWithEvents(&events)},
		Options{DryRun: false}, Accepts{})
	if err == nil {
		t.Fatal("broken-fence run = nil, want refusal")
	}
	if !errors.Is(err, errFenceBoom) {
		t.Fatalf("broken-fence run = %v, want the fence error", err)
	}
	for _, e := range events {
		if e == "collect" {
			t.Fatalf("broken-fence run collected: %v", events)
		}
	}
}
