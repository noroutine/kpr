package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// inspect is the magnifier: the full row for one exact repo:tag. A
// missing row refuses naming it (typo-proof, like plan add) —
// guessing would show the wrong row's ages.
func TestStoreInspectShowsFullRow(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	if err := runStoreInspect(cliCtx(), &out, s, "scratch:10m", false); err != nil {
		t.Fatalf("runStoreInspect: %v", err)
	}
	for _, want := range []string{"scratch", "10m", "sha256:bbb", "ttl:10m elapsed", "receiver"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("inspect missing %q:\n%s", want, out.String())
		}
	}
}

func TestStoreInspectUnknownRefuses(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	if err := runStoreInspect(cliCtx(), &out, s, "app:nope", false); err == nil {
		t.Fatal("inspect of untracked row should refuse")
	} else if !strings.Contains(err.Error(), "app:nope") {
		t.Errorf("refusal should name the row, got: %v", err)
	}
}

func TestStoreInspectMalformedRefuses(t *testing.T) {
	var out bytes.Buffer
	if err := runStoreInspect(cliCtx(), &out, store.NewMemStore(), "notaref", false); err == nil {
		t.Fatal("inspect without repo:tag should refuse")
	}
}

// Exact means exact: a glob is a malformed ref here, not a
// near-miss — plan add/remove own the pattern world.
func TestStoreWildcardRefuses(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	if err := runStoreInspect(cliCtx(), &out, s, "app*:v1", false); err == nil {
		t.Error("inspect with a glob should refuse")
	} else if !strings.Contains(err.Error(), "exact") {
		t.Errorf("glob refusal should say exact, got: %v", err)
	}
	// Glob refusal precedes every gate (splitRef first), so the
	// tokens never get read — nils document they are unreachable.
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:*"}, false, &sweep.Sweeper{Store: s}, nil, nil); err == nil {
		t.Error("rm with a glob should refuse")
	} else if !strings.Contains(err.Error(), "exact") {
		t.Errorf("glob refusal should say exact, got: %v", err)
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("refused rm deleted rows: %d left, want 2", len(rows))
	}
}

// Inspecting against dead state fails, and --json renders the full
// row for piping. If this fails, outages inspect as absent (or
// piping inspects nothing).
func TestStoreInspectDeadAndJSON(t *testing.T) {
	if err := runStoreInspect(cliCtx(), io.Discard, deadStore{}, "app:v1", false); err == nil {
		t.Error("inspect on dead store succeeded, want an error")
	}
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	if err := runStoreInspect(cliCtx(), &out, s, "app:v1", true); err != nil {
		t.Fatalf("inspect --json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("inspect --json is not JSON: %v", err)
	}
	if decoded["repo"] != "app" || decoded["tag"] != "v1" {
		t.Errorf("inspect --json = %v, want the app:v1 row", decoded)
	}
}
