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

// Row-less control events render without the repo:tag prefix: a
// stray ": — " reads as a malformed row. If this fails, fence
// flips started printing as broken rows.
func TestRenderOutcomeSkipsEmptyRef(t *testing.T) {
	got := renderOutcome(cliNow, store.Outcome{Outcome: "deny_engage", Reason: "store locked", At: cliNow.Add(-time.Minute)})
	if strings.Contains(got, ":") {
		t.Errorf("row-less outcome = %q, want no repo:tag prefix", got)
	}
	row := renderOutcome(cliNow, store.Outcome{Repo: "a", Tag: "b", Outcome: "deleted", Reason: "r", At: cliNow.Add(-90 * time.Second)})
	if !strings.HasPrefix(row, "  a:b — deleted (r)") {
		t.Errorf("row outcome = %q, want the repo:tag shape kept", row)
	}
}

// Ring entries read human: a short age trails every line, precise
// stamps staying in --json. A zero stamp degrades to words, never
// a million-hour duration. If this fails, the operator does
// timestamp arithmetic again.
func TestRenderOutcomeShowsHumanAge(t *testing.T) {
	row := renderOutcome(cliNow, store.Outcome{Repo: "a", Tag: "b", Outcome: "deleted",
		Reason: "untag", At: cliNow.Add(-90 * time.Second)})
	if row != "  a:b — deleted (untag), 1m30s ago" {
		t.Errorf("row outcome = %q, want the human age trailed", row)
	}
	flip := renderOutcome(cliNow, store.Outcome{Outcome: "deny_engage",
		Reason: "store locked", At: cliNow.Add(-time.Hour)})
	if flip != "  deny_engage (store locked), 1h0m0s ago" {
		t.Errorf("row-less outcome = %q, want the human age trailed", flip)
	}
	zero := renderOutcome(cliNow, store.Outcome{Outcome: "deleted", Reason: "untag"})
	if !strings.HasSuffix(zero, "unknown age") {
		t.Errorf("zero-stamp outcome = %q, want words not a huge duration", zero)
	}
}

// A pipe breaking on an activity line surfaces the error: the
// headers already printed, so only the per-row check catches it.
// If this fails, a truncated activity reads as complete.
func TestStoreStatusSurfacesActivityLineWriteError(t *testing.T) {
	s := store.NewMemStore()
	_ = s.PushActivity(cliCtx(), store.Outcome{Repo: "app", Tag: "v1",
		Reason: "ttl", Outcome: "deleted", At: cliNow})
	ts := cliNow.Format(time.RFC3339)
	if err := runStoreStatus(cliCtx(), &failAfterWriter{n: 2}, s,
		stubProofAPI{id: "test-id", ts: ts}, "mem (tests only)", false); err == nil {
		t.Error("store status failing on the activity line succeeded, want an error")
	}
}

// `store status` owns the store card the banner gave up: backend,
// intent, live proof, and the lineage pairing the verdicts judge
// against. If this fails, operators cannot see what the store is
// paired to — or whether gc will run at all.
func TestStoreStatusShowsLockProofIdentity(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	if err := s.SetUnlocked(cliCtx(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	if err := s.SetIdentity(cliCtx(), store.Identity{ID: "01a0f825-lineage", BaselineGen: "019-proof"}); err != nil {
		t.Fatalf("stage identity: %v", err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := s.PushActivity(cliCtx(), store.Outcome{Repo: "scratch", Tag: "10m",
		Reason: "ttl:10m elapsed", Outcome: "deleted", At: cliNow}); err != nil {
		t.Fatalf("stage activity: %v", err)
	}
	var out bytes.Buffer
	if err := runStoreStatus(cliCtx(), &out, s, stubProofAPI{ts: ts}, "mem (tests only)", false); err != nil {
		t.Fatalf("runStoreStatus: %v", err)
	}
	for _, want := range []string{"store: mem (tests only)", "store-lock: unlocked",
		"proof: 019-proof", "ago)", "identity: 01a0f825-lineage", "baseline 019-proof",
		"activity (last 1 of 1):", "scratch:10m — deleted (ttl:10m elapsed)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("store status missing %q:\n%s", want, out.String())
		}
	}
}

func TestStoreStatusFreshStore(t *testing.T) {
	var out bytes.Buffer
	if err := runStoreStatus(cliCtx(), &out, store.NewMemStore(), nil, "mem (tests only)", false); err != nil {
		t.Fatalf("runStoreStatus: %v", err)
	}
	for _, want := range []string{"store-lock: locked", "proof: unproven", "identity: unpaired", "activity: none recorded"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("fresh store status missing %q:\n%s", want, out.String())
		}
	}
}

// The status headline shares analyze's trust word: unpaired on a
// fresh store, paired when the dry verdict over live reads holds.
// It leads the card so mistrust reads before the numbers. If this
// fails, status and analyze judge by different words.
func TestStoreStatusHeadsWithTrust(t *testing.T) {
	var fresh bytes.Buffer
	if err := runStoreStatus(cliCtx(), &fresh, store.NewMemStore(), nil, "mem (tests only)", false); err != nil {
		t.Fatalf("fresh status: %v", err)
	}
	if !strings.HasPrefix(fresh.String(), "status: unpaired\n") {
		t.Errorf("fresh status heads %q, want status: unpaired first", fresh.String())
	}
	s := store.NewMemStore()
	if err := s.SetIdentity(cliCtx(), store.Identity{ID: "lin", BaselineGen: "019-proof"}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	if err := s.Record(cliCtx(), policy.Row{Repo: "noroutine/kpr-sentinel", Tag: "019-proof",
		Digest: "sha256:ccc", MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt: time.Now().UTC(), Actor: "kpr-unlock"}); err != nil {
		t.Fatalf("track served gen: %v", err)
	}
	var out bytes.Buffer
	api := stubProofAPI{ts: time.Now().UTC().Format(time.RFC3339), id: "lin"}
	if err := runStoreStatus(cliCtx(), &out, s, api, "mem (tests only)", false); err != nil {
		t.Fatalf("paired status: %v", err)
	}
	if !strings.HasPrefix(out.String(), "status: paired\n") {
		t.Errorf("paired status heads %q, want status: paired first", out.String())
	}
}

func TestStoreStatusJSON(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	if err := s.SetIdentity(cliCtx(), store.Identity{ID: "01a0f825-lineage"}); err != nil {
		t.Fatalf("stage identity: %v", err)
	}
	var out bytes.Buffer
	if err := runStoreStatus(cliCtx(), &out, s, nil, "mem (tests only)", true); err != nil {
		t.Fatalf("runStoreStatus json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("store status json unparseable: %v\n%s", err, out.String())
	}
	if got["identity"] != "01a0f825-lineage" || got["lock"] != "locked" {
		t.Errorf("store status json = %v, want identity + lock", got)
	}
}

// An unreadable lineage voices unknown, never unpaired: the card
// must not invent a pairing. If this fails, an outage reads as
// never-paired.
func TestIdentityStateUnknownOnOutage(t *testing.T) {
	if got := identityState(cliCtx(), deadStore{}); got != "unknown" {
		t.Errorf("identityState on outage = %q, want unknown", got)
	}
	if got := identityState(cliCtx(), store.NewMemStore()); got != "unpaired" {
		t.Errorf("identityState fresh = %q, want unpaired", got)
	}
}

// Status --json with activity renders the ring: the piped card
// carries what the text card prints. If this fails, --json drops
// the outcomes the counters count.
func TestStoreStatusJSONRendersActivity(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	_ = s.PushActivity(cliCtx(), store.Outcome{Repo: "app", Tag: "v1",
		Reason: "ttl elapsed", Outcome: "deleted", At: cliNow})
	var out bytes.Buffer
	if err := runStoreStatus(cliCtx(), &out, s, nil, "file (x)", true); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("status --json is not JSON: %v", err)
	}
	acts, _ := decoded["activity"].([]any)
	if len(acts) != 1 {
		t.Errorf("activity = %v, want the one pushed outcome", decoded["activity"])
	}
}

// Every status line is a real write: header, unknown-ring,
// activity header each surface the break. If this fails, a broken
// card reads as healthy.
func TestStoreStatusWriteFailuresSurface(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	_ = s.PushActivity(cliCtx(), store.Outcome{Repo: "app", Tag: "v1",
		Reason: "ttl elapsed", Outcome: "deleted", At: cliNow})
	for _, tc := range []struct {
		name  string
		store store.Store
		n     int
	}{
		{"header", s, 0},
		{"unknown ring", deadStore{}, 1},
		{"activity header", s, 1},
		{"activity row", s, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runStoreStatus(cliCtx(), &failAfterWriter{n: tc.n}, tc.store, nil, "file (x)", false); err == nil {
				t.Errorf("status %s into breaking pipe succeeded, want an error", tc.name)
			}
		})
	}
}
