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
