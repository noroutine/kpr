package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

func seedRows(s *store.MemStore) {
	c := cliCtx()
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:aaa",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-2 * time.Hour), Actor: "receiver"})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:bbb",
		PushedAt: cliNow.Add(-time.Hour), Actor: "receiver",
		Due: true, Reason: "ttl:10m elapsed"})
}

// ls is the lay of the field: every tracked row, not just the due
// ones plan shows. If this fails, operators cannot see what the
// store actually holds.
func TestStoreLsListsAllRows(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, false); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	for _, want := range []string{"app:v1", "scratch:10m", "sha256:aaa", "sha256:bbb", "due: ttl:10m elapsed", "not due"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ls missing %q:\n%s", want, out.String())
		}
	}
}

func TestStoreLsEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, store.NewMemStore(), false); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	if !strings.Contains(out.String(), "no tracked rows") {
		t.Errorf("empty ls should say so:\n%s", out.String())
	}
}

func TestStoreLsJSON(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, true); err != nil {
		t.Fatalf("runStoreLs json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("ls json unparseable: %v\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("ls json has %d rows, want 2", len(rows))
	}
}

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
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:*"}); err == nil {
		t.Error("rm with a glob should refuse")
	} else if !strings.Contains(err.Error(), "exact") {
		t.Errorf("glob refusal should say exact, got: %v", err)
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("refused rm deleted rows: %d left, want 2", len(rows))
	}
}

// rm drops the tracked row only: the registry tag survives,
// untracked until a re-push or backfill re-tracks it. The warning
// is the point — a silent rm would look like a delete.
func TestStoreRmDropsRowWarnsTagStays(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}); err != nil {
		t.Fatalf("runStoreRm: %v", err)
	}
	if !strings.Contains(out.String(), "untracked") {
		t.Errorf("rm should warn the tag stays untracked:\n%s", out.String())
	}
	rows, err := s.All(cliCtx())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, r := range rows {
		if r.Repo == "app" && r.Tag == "v1" {
			t.Error("rm left the row behind")
		}
	}
	if len(rows) != 1 {
		t.Errorf("rm took %d rows, want 1 left", len(rows))
	}
}

// rm is all-or-nothing: one unknown ref refuses before anything is
// deleted, so a typo cannot half-clear the store.
func TestStoreRmUnknownRefusesBeforeDeleting(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1", "app:nope"}); err == nil {
		t.Fatal("rm with an unknown ref should refuse")
	} else if !strings.Contains(err.Error(), "app:nope") {
		t.Errorf("refusal should name the unknown row, got: %v", err)
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("refused rm deleted rows: %d left, want 2", len(rows))
	}
}
