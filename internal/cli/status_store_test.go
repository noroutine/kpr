package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/store"
)

// status store prints the store card from the shared model: one
// line per field, the fs line only when the card voices size. If
// this fails, the CLI and the console read different cards.
func TestStatusStorePrintsCardLines(t *testing.T) {
	s := store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	var buf bytes.Buffer
	if err := runStatusStore(context.Background(), &buf, s, "", 0, false, false); err != nil {
		t.Fatalf("runStatusStore: %v", err)
	}
	for _, want := range []string{
		"store: mem (in-memory, tests only)",
		"health: reachable",
		"standing: ok",
		"lock: unlocked",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("card missing %q:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "fs:") {
		t.Errorf("mem card voices fs line:\n%s", buf.String())
	}
}

// status store --json is the card struct verbatim: machine
// readers get the standing code, not the words. If this fails,
// --json drifts from the model both renderers sign.
func TestStatusStoreJSONIsCardVerbatim(t *testing.T) {
	s := store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	var buf bytes.Buffer
	if err := runStatusStore(context.Background(), &buf, s, "", 0, false, true); err != nil {
		t.Fatalf("runStatusStore: %v", err)
	}
	var got struct {
		Standing string `json:"standing"`
		Name     string `json:"name"`
		LockNote string `json:"lockNote"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Standing != "locked" || got.Name != "mem" || got.LockNote != "locked" {
		t.Errorf("json = %+v, want locked mem card", got)
	}
}
