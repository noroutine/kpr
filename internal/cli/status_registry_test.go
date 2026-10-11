package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// stubRegProbe answers the reachability probe without HTTP.
type stubRegProbe struct{ err error }

func (s stubRegProbe) Reachable(context.Context) error { return s.err }

// status registry voices reachability with the probed endpoint
// from the shared model: the console's Registry section reads
// the same card. A down registry still names its URL. If this
// fails, the CLI and the console probe different registries.
func TestStatusRegistryVoicesEndpoint(t *testing.T) {
	ctx := context.Background()
	card := keeper.FetchRegistryCard(ctx, stubRegProbe{}, "http://registry:5000")
	var buf bytes.Buffer
	if err := renderRegistryCard(&buf, card, false); err != nil {
		t.Fatalf("renderRegistryCard: %v", err)
	}
	for _, want := range []string{"registry: reachable", "url: http://registry:5000"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("card missing %q:\n%s", want, buf.String())
		}
	}
	down := keeper.FetchRegistryCard(ctx, stubRegProbe{err: errors.New("connection refused")}, "http://registry:5000")
	buf.Reset()
	if err := renderRegistryCard(&buf, down, false); err != nil {
		t.Fatalf("renderRegistryCard: %v", err)
	}
	for _, want := range []string{"registry: unreachable", "url: http://registry:5000"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("down card missing %q:\n%s", want, buf.String())
		}
	}
}

// status registry --json is the card struct verbatim: machine
// readers get the standing code, not the words. If this fails,
// --json drifts from the model both renderers sign.
func TestStatusRegistryJSONIsCardVerbatim(t *testing.T) {
	card := keeper.FetchRegistryCard(context.Background(), stubRegProbe{}, "http://registry:5000")
	var buf bytes.Buffer
	if err := renderRegistryCard(&buf, card, true); err != nil {
		t.Fatalf("renderRegistryCard: %v", err)
	}
	var got struct {
		Standing  string `json:"standing"`
		URL       string `json:"url"`
		Reachable bool   `json:"reachable"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Standing != "ok" || !got.Reachable || got.URL != "http://registry:5000" {
		t.Errorf("json = %+v, want ok reachable card", got)
	}
}
