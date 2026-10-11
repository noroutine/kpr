package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// status gateway prints the fence posture from the shared
// model: open/deny/held with the endpoints beside it. If this
// fails, the CLI and the console read different fences.
func TestStatusGatewayPrintsPosture(t *testing.T) {
	s := store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	card := keeper.FetchGatewayCard(context.Background(), stubGateposture{}, s, ":5000", "http://registry:5000")
	var buf bytes.Buffer
	if err := renderGatewayCard(&buf, card, false); err != nil {
		t.Fatalf("renderGatewayCard: %v", err)
	}
	for _, want := range []string{
		"edge: open",
		"fence: deny",
		":5000 → http://registry:5000",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("card missing %q:\n%s", want, buf.String())
		}
	}
}

// A closed edge renders no fence verdict: the posture was never
// fetched, and printing "pass" would fabricate one. The console
// guards its Fence card behind the same open check. If this
// fails, the CLI reads a fence the console refuses to show.
func TestStatusGatewayClosedHidesFence(t *testing.T) {
	card := keeper.GatewayCard{Standing: keeper.StandingClosed}
	var buf bytes.Buffer
	if err := renderGatewayCard(&buf, card, false); err != nil {
		t.Fatalf("renderGatewayCard: %v", err)
	}
	if !strings.Contains(buf.String(), "edge: closed") {
		t.Errorf("closed card missing edge state:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "fence:") {
		t.Errorf("closed card fabricates a fence verdict:\n%s", buf.String())
	}
}

// The text view voices what the posture means, not just its
// name: deny refuses writes, pass allows them. The names alone
// are the model's shorthand (--json keeps them); the CLI owes
// the effect. If this fails, the renderer echoes internals.
func TestStatusGatewayVoicesEffect(t *testing.T) {
	s := store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	card := keeper.FetchGatewayCard(context.Background(), stubGateposture{}, s, ":5000", "http://registry:5000")
	var buf bytes.Buffer
	if err := renderGatewayCard(&buf, card, false); err != nil {
		t.Fatalf("renderGatewayCard: %v", err)
	}
	if want := "fence: deny — writes refused"; !strings.Contains(buf.String(), want) {
		t.Errorf("card missing %q:\n%s", want, buf.String())
	}
	open := store.NewMemStore()
	if err := open.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	var passing bytes.Buffer
	passingCard := keeper.FetchGatewayCard(context.Background(), stubGateposture{}, open, ":5000", "http://registry:5000")
	if err := renderGatewayCard(&passing, passingCard, false); err != nil {
		t.Fatalf("renderGatewayCard: %v", err)
	}
	if want := "fence: pass — writes allowed"; !strings.Contains(passing.String(), want) {
		t.Errorf("card missing %q:\n%s", want, passing.String())
	}
}

// status gateway --json is the card struct verbatim: machine
// readers get the standing code, not the words. If this fails,
// --json drifts from the model both renderers sign.
func TestStatusGatewayJSONIsCardVerbatim(t *testing.T) {
	card := keeper.GatewayCard{Standing: keeper.StandingClosed}
	var buf bytes.Buffer
	if err := renderGatewayCard(&buf, card, true); err != nil {
		t.Fatalf("renderGatewayCard: %v", err)
	}
	var got struct {
		Standing string `json:"standing"`
		Open     bool   `json:"open"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Standing != "closed" || got.Open {
		t.Errorf("json = %+v, want closed card", got)
	}
}

// stubGateposture pins deny/held without a serve loop.
type stubGateposture struct{}

func (stubGateposture) Snapshot() (bool, bool) { return false, false }
