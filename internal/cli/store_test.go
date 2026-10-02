package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
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

func seedSentinel(s *store.MemStore) {
	_ = s.Record(cliCtx(), policy.Row{Repo: "noroutine/kpr-sentinel", Tag: "019-gen",
		Digest: "sha256:ccc", PushedAt: cliNow.Add(-30 * time.Minute), Actor: "kpr-unlock"})
}

// mustUnlock opens the marker: rm tests stage unlocked ground so
// the intent gate (tested here, not there) stays green.
func mustUnlock(t *testing.T, s *store.MemStore) {
	t.Helper()
	if err := s.SetUnlocked(cliCtx(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
}

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

// unlockedProof reads the marker the way the rm command does.
func unlockedProof(t *testing.T, s *store.MemStore) proof.UnlockedStore {
	t.Helper()
	unlocked, err := proof.ProveUnlockedStore(cliCtx(), s)
	if err != nil {
		t.Fatalf("prove unlocked ground: %v", err)
	}
	return unlocked
}

// untagProof mints the way the rm command does: paired store,
// served generation of our lineage. If minting fails here, the
// test ground (not the rm path) is broken.
func untagProof(t *testing.T, s *store.MemStore) proof.SameStore {
	t.Helper()
	if err := s.SetIdentity(cliCtx(), store.Identity{ID: "test-id"}); err != nil {
		t.Fatalf("stage identity: %v", err)
	}
	mustUnlock(t, s)
	same, err := proof.Prover{
		Sentinel: stubProofAPI{id: "test-id", ts: cliNow.Format(time.RFC3339)},
		Store:    s, DryRun: false,
		Now: func() time.Time { return cliNow },
	}.Prove(cliCtx())
	if err != nil {
		t.Fatalf("prove on paired ground: %v", err)
	}
	return same
}

// ls is the lay of the field: tracked rows, short columns, no
// machinery. Sentinels stay out by default (semantically
// different); `ls sentinels` shows only them. If this fails,
// operators cannot see what the store actually holds.
func TestStoreLsListsRowsShort(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	// Same namespace, not machinery: must stay visible.
	_ = s.Record(cliCtx(), policy.Row{Repo: "noroutine/kpr-web", Tag: "v2", Digest: "sha256:ddd",
		PushedAt: cliNow.Add(-10 * time.Minute), Actor: "receiver"})
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	for _, want := range []string{"REPO:TAG", "AGE", "DUE", "app:v1", "2h0m0s ago",
		"scratch:10m", "1h0m0s ago", "ttl:10m elapsed", "not due", "noroutine/kpr-web:v2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ls missing %q:\n%s", want, out.String())
		}
	}
	for _, gone := range []string{"kpr-sentinel", "sha256:aaa", "receiver", "pushed=", "actor="} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("ls leaks %q (sentinel/digest/actor/prefix):\n%s", gone, out.String())
		}
	}
}

func TestStoreLsSentinelsOnly(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow, sentinels: true}); err != nil {
		t.Fatalf("runStoreLs sentinels: %v", err)
	}
	if !strings.Contains(out.String(), "noroutine/kpr-sentinel:019-gen") {
		t.Errorf("sentinels view missing the generation:\n%s", out.String())
	}
	for _, gone := range []string{"app:v1", "scratch:10m"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("sentinels view leaks user row %q:\n%s", gone, out.String())
		}
	}
}

func TestStoreLsLong(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow, long: true}); err != nil {
		t.Fatalf("runStoreLs long: %v", err)
	}
	for _, want := range []string{"DIGEST", "PUSHED", "ACTOR", "sha256:aaa", "receiver", "2026-09-27T10:00:00Z"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("long ls missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "kpr-sentinel") {
		t.Errorf("long ls leaks sentinels by default:\n%s", out.String())
	}
}

func TestStoreLsEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, store.NewMemStore(), storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	if !strings.Contains(out.String(), "no tracked rows") {
		t.Errorf("empty ls should say so:\n%s", out.String())
	}
	var sout bytes.Buffer
	if err := runStoreLs(cliCtx(), &sout, store.NewMemStore(), storeLsOpts{now: cliNow, sentinels: true}); err != nil {
		t.Fatalf("runStoreLs sentinels: %v", err)
	}
	if !strings.Contains(sout.String(), "no sentinel rows") {
		t.Errorf("empty sentinels view should say so:\n%s", sout.String())
	}
}

func TestStoreLsJSON(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow, json: true}); err != nil {
		t.Fatalf("runStoreLs json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("ls json unparseable: %v\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("ls json has %d rows, want 2 (sentinels excluded)", len(rows))
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

// untagStub is the registry half of `rm --untag`: canned outcome
// per call, with the refs it saw (untouched on refusal).
type untagStub struct {
	outcome string
	err     error
	calls   []string
}

func (s *untagStub) DeleteManifest(_ context.Context, repo, ref string) (string, error) {
	s.calls = append(s.calls, repo+":"+ref)
	return s.outcome, s.err
}

// flipStub succeeds once, then holds: partial success must still
// print the confirmed row before the error returns.
type flipStub struct {
	calls []string
	n     int
}

func (s *flipStub) DeleteManifest(_ context.Context, repo, ref string) (string, error) {
	s.calls = append(s.calls, repo+":"+ref)
	s.n++
	if s.n == 1 {
		return "deleted", nil
	}
	return "held", nil
}

// A pipe breaking mid-list surfaces the error: the first removed
// line already printed, so only the per-row check catches it. If
// this fails, a truncated removal list reads as complete.
func TestStoreRmSurfacesRowLineWriteError(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	if err := s.SetUnlocked(cliCtx(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	if err := runStoreRm(cliCtx(), &failAfterWriter{n: 1}, s,
		[]string{"app:v1", "scratch:10m"}, false,
		&sweep.Sweeper{Store: s}, nil, unlockedProof(t, s)); err == nil {
		t.Error("rm failing on the row line succeeded, want an error")
	}
}

func TestStoreRmUntagPartialPrintsDone(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	stub := &flipStub{}
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1", "scratch:10m"}, true,
		&sweep.Sweeper{Store: s, Registry: stub}, untagProof(t, s), unlockedProof(t, s)); err == nil {
		t.Fatal("partial untag succeeded, want the held failure")
	}
	if !strings.Contains(out.String(), "untagged app:v1") {
		t.Errorf("partial untag hid the confirmed row:\n%s", out.String())
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the 1 held row kept", len(rows))
	}
}

// --untag deletes the manifest by digest first and drops the row
// only on confirm — the sweeper's order, without the due mark.
// If this fails, rm either orphans registry tags or drops rows
// for tags that survive.
func TestStoreRmUntagDeletesThenDropsRow(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	stub := &untagStub{outcome: "deleted"}
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, true, &sweep.Sweeper{Store: s, Registry: stub}, untagProof(t, s), unlockedProof(t, s)); err != nil {
		t.Fatalf("runStoreRm untag: %v", err)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "app:sha256:aaa" {
		t.Errorf("untag calls = %v, want [app:sha256:aaa] (by digest, not tag)", stub.calls)
	}
	rows, _ := s.All(cliCtx())
	for _, r := range rows {
		if r.Repo == "app" && r.Tag == "v1" {
			t.Error("untagged row left behind")
		}
	}
	if !strings.Contains(out.String(), "untagged") {
		t.Errorf("rm should say untagged:\n%s", out.String())
	}
}

// The untag path without a token refuses before the first
// manifest: runStoreRm threads the proof through, it never mints
// one. If this fails, the command layer can delete unproven.
func TestStoreRmUntagWithoutProofRefuses(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	// Unlocked ground: this test isolates the missing identity
	// token (locked refusal has its own test below).
	mustUnlock(t, s)
	stub := &untagStub{outcome: "deleted"}
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, true,
		&sweep.Sweeper{Store: s, Registry: stub}, nil, unlockedProof(t, s)); err == nil {
		t.Fatal("proofless rm --untag succeeded, want the blind refusal")
	}
	if len(stub.calls) != 0 {
		t.Errorf("proofless rm --untag attempted %d registry deletes", len(stub.calls))
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("rows = %d, want both kept", len(rows))
	}
}

// Locked refuses before resolution: a locked store forgets nothing
// and deletes nothing. The nil unlocked token IS the locked store
// here — the command mints it via ProveUnlockedStore, which fails
// on a locked marker, so no caller can hold a token for one. If
// this fails, the freeze has a hole at the command layer.
func TestStoreRmLockedRefuses(t *testing.T) {
	for _, untag := range []bool{false, true} {
		s := store.NewMemStore()
		seedRows(s)
		// Paired but locked: identity reads need no marker, so the
		// same-token mints and only intent is missing.
		if err := s.SetIdentity(cliCtx(), store.Identity{ID: "test-id"}); err != nil {
			t.Fatalf("stage identity: %v", err)
		}
		same, err := proof.Prover{
			Sentinel: stubProofAPI{id: "test-id", ts: cliNow.Format(time.RFC3339)},
			Store:    s, DryRun: false,
			Now: func() time.Time { return cliNow },
		}.Prove(cliCtx())
		if err != nil {
			t.Fatalf("prove on paired ground: %v", err)
		}
		stub := &untagStub{outcome: "deleted"}
		var out bytes.Buffer
		if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, untag,
			&sweep.Sweeper{Store: s, Registry: stub}, same, nil); err == nil {
			t.Fatalf("untag=%v locked rm succeeded, want the locked refusal", untag)
		} else if !strings.Contains(err.Error(), "locked") {
			t.Errorf("untag=%v err = %q, want the locked refusal named", untag, err.Error())
		}
		if len(stub.calls) != 0 {
			t.Errorf("untag=%v locked rm attempted %d registry deletes", untag, len(stub.calls))
		}
		rows, _ := s.All(cliCtx())
		if len(rows) != 2 {
			t.Errorf("untag=%v rows = %d, want both kept", untag, len(rows))
		}
	}
}

// A held delete (tag fallback on distribution:3, denied configs)
// keeps the row: dropping it would untrack a live tag silently.
func TestStoreRmUntagHeldKeepsRowLoud(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	stub := &untagStub{outcome: "held"}
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, true, &sweep.Sweeper{Store: s, Registry: stub}, untagProof(t, s), unlockedProof(t, s)); err == nil {
		t.Fatal("held untag succeeded, want a loud failure")
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("held untag dropped the row: %d left, want 2", len(rows))
	}
}

// rm drops the tracked row only: the registry tag survives,
// untracked until a re-push or backfill re-tracks it. The warning
// is the point — a silent rm would look like a delete.
func TestStoreRmDropsRowWarnsTagStays(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	mustUnlock(t, s)
	var out bytes.Buffer
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1"}, false, &sweep.Sweeper{Store: s}, nil, unlockedProof(t, s)); err != nil {
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
	// Unknown-ref refusal precedes every gate (resolution first), so
	// the tokens never get read — nils document they are unreachable.
	if err := runStoreRm(cliCtx(), &out, s, []string{"app:v1", "app:nope"}, false, &sweep.Sweeper{Store: s}, nil, nil); err == nil {
		t.Fatal("rm with an unknown ref should refuse")
	} else if !strings.Contains(err.Error(), "app:nope") {
		t.Errorf("refusal should name the unknown row, got: %v", err)
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 2 {
		t.Errorf("refused rm deleted rows: %d left, want 2", len(rows))
	}
}
