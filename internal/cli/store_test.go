package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
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

// Same-repo rows sort by tag: the repo guard routes equal repos
// to the tag compare, and the tag compare orders them. Seeded
// backwards, so store order alone would fail. If this fails, ls
// lists tags in store order — the plan view scrambles.
func TestStoreLsSortsSameRepoByTag(t *testing.T) {
	s := store.NewMemStore()
	c := cliCtx()
	_ = s.Record(c, policy.Row{Repo: "zzz", Tag: "v9", Digest: "sha256:ddd",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-time.Hour), Actor: "receiver"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v2", Digest: "sha256:ddd",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-time.Hour), Actor: "receiver"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:aaa",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-2 * time.Hour), Actor: "receiver"})
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	body := out.String()
	v1, v2, z := strings.Index(body, "app:v1"), strings.Index(body, "app:v2"), strings.Index(body, "zzz:v9")
	if v1 < 0 || v2 < 0 || z < 0 || v1 > v2 || v2 > z {
		t.Errorf("rows out of repo-then-tag order:\n%s", body)
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

// A future push clamps to zero age: the row is odd, the rendering
// must not be. If this fails, clock-skewed pushes print negative
// ages.
func TestShortAgeClampsFuture(t *testing.T) {
	if got := shortAge(cliNow, cliNow.Add(time.Hour)); got != "0s ago" {
		t.Errorf("shortAge(future) = %q, want 0s ago", got)
	}
}

// Listing against dead state fails naming the outage: an empty
// table must mean empty, never unreadable. If this fails, an
// outage prints as no inventory.
func TestStoreLsOnDeadStoreFails(t *testing.T) {
	if err := runStoreLs(cliCtx(), io.Discard, deadStore{}, storeLsOpts{now: cliNow}); err == nil {
		t.Error("ls on dead store succeeded, want an error")
	}
}

// Every ls line is a real write: a breaking pipe surfaces the
// failure at the header or the row, long or short. If this fails,
// truncated tables read as complete inventory.
func TestStoreLsWriteFailuresSurface(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	for _, tc := range []struct {
		name string
		opts storeLsOpts
		n    int
	}{
		{"long header", storeLsOpts{now: cliNow, long: true}, 0},
		{"long row", storeLsOpts{now: cliNow, long: true}, 1},
		{"short header", storeLsOpts{now: cliNow}, 0},
		{"short row", storeLsOpts{now: cliNow}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runStoreLs(cliCtx(), &failAfterWriter{n: tc.n}, s, tc.opts); err == nil {
				t.Errorf("ls %s into breaking pipe succeeded, want an error", tc.name)
			}
		})
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

// Removing against dead state fails before resolution: nothing
// resolves, nothing drops. If this fails, an outage deletes on
// paper.
func TestStoreRmOnDeadStoreFails(t *testing.T) {
	stub := &untagStub{outcome: "deleted"}
	if err := runStoreRm(cliCtx(), io.Discard, deadStore{}, []string{"app:v1"}, false,
		&sweep.Sweeper{Store: deadStore{}, Registry: stub}, nil, unlockedProof(t, mustUnlockedMem(t))); err == nil {
		t.Error("rm on dead store succeeded, want an error")
	}
}

// An untagged row that cannot print still drops: the registry
// already confirmed, so the failure is loud, not a rollback. If
// this fails, a breaking pipe resurrects deleted rows.
func TestStoreRmUntagRowWriteFailureSurfaces(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	stub := &untagStub{outcome: "deleted"}
	if err := runStoreRm(cliCtx(), &failAfterWriter{}, s, []string{"app:v1"}, true,
		&sweep.Sweeper{Store: s, Registry: stub}, untagProof(t, s), unlockedProof(t, s)); err == nil {
		t.Error("untag rm into breaking pipe succeeded, want an error")
	}
	rows, _ := s.All(cliCtx())
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1 (confirmed drop stands)", len(rows))
	}
}

// mustUnlockedMem stages unlocked mem ground for proofs that need
// a store apart from the row table.
func mustUnlockedMem(t *testing.T) *store.MemStore {
	t.Helper()
	s := store.NewMemStore()
	mustUnlock(t, s)
	return s
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

// Command tails run the real RunE against a file backend: ls
// (valid and curious spellings), inspect, rm refusals. Refusals
// are the point here — rm-proof wiring already has success paths —
// plus one rm tail past the gates. If any fail, the command is
// unwired.
func TestStoreCommandTailsRunAgainstFileBackend(t *testing.T) {
	dir := t.TempDir()
	regRoot := t.TempDir()
	srv := serveRegistry(t, regRoot, true)
	defer srv.Close()
	cfg := stageGCStore(t, regRoot)
	setUnlockConfig(t, cfg)
	if _, err := runLockCmd(t, dir, srv.URL, unlockCmd); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	for _, tc := range []struct {
		name    string
		target  *cobra.Command
		args    []string
		wantErr string
	}{
		{"ls", storeLsCmd, nil, ""},
		{"ls sentinels", storeLsCmd, []string{"sentinels"}, ""},
		{"inspect unknown", storeInspectCmd, []string{"ghost:v1"}, "no tracked row"},
		{"rm unknown", storeRmCmd, []string{"ghost:v1"}, "no tracked row"},
		{"rm untag unknown", storeRmCmd, []string{"ghost:v1"}, "no tracked row"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.args) > 0 && tc.target == storeRmCmd {
				if tc.name == "rm untag unknown" {
					if err := storeRmCmd.Flags().Set("untag", "true"); err != nil {
						t.Fatalf("set --untag: %v", err)
					}
					defer func() { _ = storeRmCmd.Flags().Set("untag", "false") }()
				}
			}
			out, err := runCmdWithArgs(t, dir, srv.URL, tc.target, tc.args)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("%s err = %v, want %q (out: %s)", tc.name, err, tc.wantErr, out)
			}
		})
	}
	if err := storeLsCmd.Args(storeLsCmd, []string{"bogus", "extra"}); err == nil {
		t.Error("ls with two args validated, want refusal")
	}
	if err := storeLsCmd.Args(storeLsCmd, []string{"bogus"}); err == nil {
		t.Error("ls with curious spelling validated, want refusal")
	}
	// The two valid spellings validate clean: bare and sentinels.
	// If this fails, a boundary tweak refuses the documented
	// invocation while misspellings still error.
	for _, args := range [][]string{nil, {"sentinels"}} {
		if err := storeLsCmd.Args(storeLsCmd, args); err != nil {
			t.Errorf("ls %v refused: %v, want acceptance", args, err)
		}
	}
}

// The ls flag plumbing splits at the command, not just the unit:
// `ls sentinels` through RunE shows machinery, bare ls shows
// inventory. If this fails, the flag misroutes the view.
func TestStoreLsCommandSplitsSentinels(t *testing.T) {
	dir := t.TempDir()
	srv := serveRegistry(t, t.TempDir(), false)
	defer srv.Close()
	func() {
		t.Setenv(config.EnvStore, "file")
		t.Setenv(config.EnvStoreDir, dir)
		s, err := OpenStore(config.NewBuilder().FromEnv().Build())
		if err != nil {
			t.Fatalf("open file store: %v", err)
		}
		defer func() { _ = s.Close() }()
		c := context.Background()
		_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:a", PushedAt: cliNow})
		_ = s.Record(c, policy.Row{Repo: "noroutine/kpr-sentinel", Tag: "019-gen",
			Digest: "sha256:b", PushedAt: cliNow})
	}()
	plain, err := runCmdWithArgs(t, dir, srv.URL, storeLsCmd, nil)
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(plain, "app:v1") || strings.Contains(plain, "kpr-sentinel") {
		t.Errorf("ls through the command mixes the views:\n%s", plain)
	}
	mach, err := runCmdWithArgs(t, dir, srv.URL, storeLsCmd, []string{"sentinels"})
	if err != nil {
		t.Fatalf("ls sentinels: %v", err)
	}
	if !strings.Contains(mach, "kpr-sentinel") || strings.Contains(mach, "app:v1") {
		t.Errorf("ls sentinels through the command mixes the views:\n%s", mach)
	}
}

// ls splits inventory from machinery: default shows tracked rows
// without sentinels, `ls sentinels` shows only them. If this
// fails, the split the console depends on collapses to one view.
func TestStoreLsSplitsSentinels(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	var plain bytes.Buffer
	if err := runStoreLs(cliCtx(), &plain, s, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(plain.String(), "app:v1") {
		t.Errorf("ls lacks the tracked row:\n%s", plain.String())
	}
	if strings.Contains(plain.String(), "kpr-sentinel") {
		t.Errorf("ls leaks machinery:\n%s", plain.String())
	}
	var mach bytes.Buffer
	if err := runStoreLs(cliCtx(), &mach, s, storeLsOpts{now: cliNow, sentinels: true}); err != nil {
		t.Fatalf("ls sentinels: %v", err)
	}
	if !strings.Contains(mach.String(), "kpr-sentinel") {
		t.Errorf("ls sentinels lacks the generation:\n%s", mach.String())
	}
	if strings.Contains(mach.String(), "app:v1") {
		t.Errorf("ls sentinels leaks inventory:\n%s", mach.String())
	}
}

// runCmdWithArgs drives one command RunE with args against a file
// backend: the args-aware twin of runLockCmd.
func runCmdWithArgs(t *testing.T, dir, regURL string, target *cobra.Command, args []string) (string, error) {
	t.Helper()
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, dir)
	t.Setenv(config.EnvRegistryURL, regURL)
	var buf bytes.Buffer
	target.SetOut(&buf)
	defer target.SetOut(nil)
	target.SetContext(context.Background())
	err := target.RunE(target, args)
	return buf.String(), err
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
