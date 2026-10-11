package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// status clock prints the time transport from the shared model:
// method, server, live skew, bound. If this fails, the CLI and
// the console read different clocks.
func TestStatusClockPrintsTransport(t *testing.T) {
	card := keeper.ClockCard{
		Standing: keeper.StandingDegraded, Method: "https",
		Server: "https://ts:8443", Skew: 61 * time.Second, Tolerance: 30 * time.Second,
	}
	var buf bytes.Buffer
	if err := renderClockCard(&buf, card, time.Now(), false); err != nil {
		t.Fatalf("renderClockCard: %v", err)
	}
	for _, want := range []string{
		"method: https",
		"server: https://ts:8443",
		"skew: +1m1s",
		"tolerance 30s",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("card missing %q:\n%s", want, buf.String())
		}
	}
}

// status clock shows server time as kpr sees it: the render
// stamp beside the skew, same shape as the console's Server
// time card. The stamp is caller-owned (the card carries no
// Now); here it is pinned so the test stays deterministic. If
// this fails, the CLI shows a skew with no anchor to read it
// against.
func TestStatusClockPrintsServerTime(t *testing.T) {
	card := keeper.ClockCard{
		Standing: keeper.StandingOK, Method: "https",
		Server: "zeitstempel.dfn.de", Tolerance: 30 * time.Second,
	}
	now := time.Date(2026, 10, 11, 16, 35, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := renderClockCard(&buf, card, now, false); err != nil {
		t.Fatalf("renderClockCard: %v", err)
	}
	if want := "now: 2026-10-11 16:35:00 UTC"; !strings.Contains(buf.String(), want) {
		t.Errorf("card missing %q:\n%s", want, buf.String())
	}
}

// status clock --json is the card struct verbatim: machine
// readers get nanosecond skews and the standing code. If this
// fails, --json drifts from the model both renderers sign.
func TestStatusClockJSONIsCardVerbatim(t *testing.T) {
	card := keeper.ClockCard{Standing: keeper.StandingUnchecked, Method: "local", Server: "machine clock"}
	var buf bytes.Buffer
	if err := renderClockCard(&buf, card, time.Now(), true); err != nil {
		t.Fatalf("renderClockCard: %v", err)
	}
	var got struct {
		Standing string `json:"standing"`
		Method   string `json:"method"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Standing != "unchecked" || got.Method != "local" {
		t.Errorf("json = %+v, want unchecked local card", got)
	}
}
