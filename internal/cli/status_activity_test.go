package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stageActivity returns a mem store with one ring entry: the
// status activity fixture without a backend.
func stageActivity(t *testing.T) *store.MemStore {
	t.Helper()
	ctx := context.Background()
	s := store.NewMemStore()
	if err := s.PushActivity(ctx, store.Outcome{Repo: "app", Tag: "v1",
		Reason: "ttl:10m elapsed", Outcome: "deleted", At: time.Now().UTC().Add(-90 * time.Minute)}); err != nil {
		t.Fatalf("push: %v", err)
	}
	return s
}

// status activity prints the tail from the shared model, capped
// and said out loud like the console: last n of total. If this
// fails, the tail lies about the ring.
func TestStatusActivityPrintsTailTotal(t *testing.T) {
	card := keeper.FetchActivityCard(context.Background(), stageActivity(t))
	var buf bytes.Buffer
	if err := renderActivityCard(&buf, card, time.Now().UTC(), false); err != nil {
		t.Fatalf("renderActivityCard: %v", err)
	}
	for _, want := range []string{
		"activity (last 1 of 1):",
		"app:v1 — deleted (ttl:10m elapsed), 1h30m ago",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("tail missing %q:\n%s", want, buf.String())
		}
	}
}

// status activity --json is the full ring with precise stamps:
// the /api/activity shape, superset. If this fails, --json
// drifts from the model both renderers sign.
func TestStatusActivityJSONIsFullRing(t *testing.T) {
	card := keeper.FetchActivityCard(context.Background(), stageActivity(t))
	var buf bytes.Buffer
	if err := renderActivityCard(&buf, card, time.Now().UTC(), true); err != nil {
		t.Fatalf("renderActivityCard: %v", err)
	}
	var got struct {
		Total    int `json:"total"`
		Outcomes []struct {
			Repo    string `json:"repo"`
			Outcome string `json:"outcome"`
			At      string `json:"at"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 || len(got.Outcomes) != 1 || got.Outcomes[0].Outcome != "deleted" {
		t.Errorf("json = %+v, want the full ring", got)
	}
}

// A down backend reads unknown on the activity card, never an
// empty ring — silence would misreport "nothing to do". If
// this fails, the renderer drops the standing the fetch
// voiced. (Plan has no CLI card: `kpr plan` owns the human
// view, the console list reads Status.Plan.)
func TestStatusActivityVoicesUnknownOnDownStore(t *testing.T) {
	down := keeper.FetchActivityCard(context.Background(), nil)
	var buf bytes.Buffer
	if err := renderActivityCard(&buf, down, time.Now().UTC(), false); err != nil {
		t.Fatalf("renderActivityCard: %v", err)
	}
	if !strings.Contains(buf.String(), "activity: unknown") {
		t.Errorf("down activity prints:\n%s\nwant the unknown line, never an empty tail", buf.String())
	}
}
