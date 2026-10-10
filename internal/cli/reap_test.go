package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Unarmed reap only prints the plan (same source as plan): nothing is
// marked, nothing will sweep. If this fails, the default run mutates —
// the implicit-dry-run promise broken at its most important site.
func TestReapDryRunPrintsWithoutMarking(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), nil, nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	if !strings.Contains(out.String(), "scratch:10m") {
		t.Errorf("dry-run prints no plan:\n%s", out.String())
	}
	if due, _ := s.Due(context.Background()); len(due) != 0 {
		t.Errorf("dry-run marked %d rows, want 0", len(due))
	}
}

// Armed reap marks what the policies select — expired TTL here — and
// leaves :latest and fresh tags alone. If this fails, arming either
// marks nothing (sweep starves) or marks the tags it promised to keep.
func TestReapArmedMarksOnlySelected(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "20m",
		Digest: "sha256:f", PushedAt: cliNow.Add(-time.Minute)})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "latest",
		Digest: "sha256:b", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), proof.Arm(true, false), nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	due, _ := s.Due(c)
	if len(due) != 1 || due[0].Tag != "10m" {
		t.Fatalf("due = %+v, want [scratch:10m]", due)
	}
	if due[0].Reason == "" {
		t.Error("marked row carries no reason")
	}
	// Armed prints what it marked, like the dry-run plan: only
	// the trailer differs. If this fails, the modes diverge and
	// the armed run is a bare count.
	if !strings.Contains(out.String(), "scratch:10m") {
		t.Errorf("armed prints no rows:\n%s", out.String())
	}
}

// Armed reap with --exclude skips keep-N for matching qualified names:
// eleven scratch versions would lose their oldest to keep-10, but the
// operator excluded the repo. If this fails, excludes don't reach the
// only selector that reads them.
func TestReapExcludeSkipsKeepN(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	for i := 1; i <= 11; i++ {
		_ = s.Record(c, policy.Row{Repo: "scratch", Tag: fmt.Sprintf("v%d", i),
			Digest: "sha256:a", PushedAt: cliNow.Add(-time.Duration(i) * time.Hour)})
	}
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, nil, proof.Arm(true, false), []string{"^scratch:"}, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	if due, _ := s.Due(c); len(due) != 0 {
		t.Errorf("excluded repo lost %d rows to keep-N, want 0", len(due))
	}
}

// A named reap marks only that policy's rows: ttl takes the TTL
// row, hash the bare hash, partial the digest-less stale one,
// keep-n the pile's oldest. If this fails, selective reaping leaks
// across policies.
func TestReapSelectivePolicy(t *testing.T) {
	for pol, want := range map[string][2]string{
		"ttl":     {"scratch", "10m"},
		"hash":    {"scratch", "abc1234"},
		"partial": {"scratch", "partial"},
		"keep-n":  {"pile", "v01"},
	} {
		s := store.NewMemStore()
		reapStage(s)
		var out bytes.Buffer
		if err := runReap(cliCtx(), &out, s, nil, proof.Arm(true, false), nil, cliNow, pol); err != nil {
			t.Fatalf("reap %s: %v", pol, err)
		}
		due, err := s.Due(cliCtx())
		if err != nil {
			t.Fatalf("reap %s due: %v", pol, err)
		}
		if len(due) != 1 || due[0].Repo != want[0] || due[0].Tag != want[1] {
			t.Errorf("reap %s due = %v, want exactly %s:%s", pol, dueTags(due), want[0], want[1])
		}
	}
}

// Successive reaps accumulate in the plan until sweep or discard:
// reaping ttl then partial leaves both rows due. If this fails,
// a second reap wipes the first policy's marks.
func TestReapAccumulatesAcrossPolicies(t *testing.T) {
	s := store.NewMemStore()
	reapStage(s)
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, nil, proof.Arm(true, false), nil, cliNow, "ttl"); err != nil {
		t.Fatalf("reap ttl: %v", err)
	}
	if err := runReap(cliCtx(), &out, s, nil, proof.Arm(true, false), nil, cliNow, "partial"); err != nil {
		t.Fatalf("reap partial: %v", err)
	}
	due, err := s.Due(cliCtx())
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due after two reaps = %v, want 2 rows", dueTags(due))
	}
}

// An unknown policy name refuses and lists the valid ones, so a
// typo never silently reaps everything. If this fails, `reap bogus`
// either panics or reaps the world.
func TestReapUnknownPolicyRefuses(t *testing.T) {
	s := store.NewMemStore()
	reapStage(s)
	var out bytes.Buffer
	err := runReap(cliCtx(), &out, s, nil, proof.Arm(true, false), nil, cliNow, "bogus")
	if err == nil {
		t.Fatal("reap bogus succeeded, want refusal")
	}
	for _, name := range []string{"all", "ttl", "hash", "partial", "untagged", "keep-n"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q does not list policy %q", err, name)
		}
	}
	if due, _ := s.Due(cliCtx()); len(due) != 0 {
		t.Errorf("refused reap marked %v, want nothing", dueTags(due))
	}
}

// Armed reap with several selected rows marks all of them, sorted: the
// evaluate order contract must hold past a single row. If this fails,
// multi-mark passes scramble or drop candidates.
func TestReapArmedMarksAllSorted(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "zebra", Tag: "10m",
		Digest: "sha256:1", PushedAt: cliNow.Add(-time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "apple", Tag: "10m",
		Digest: "sha256:2", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	// No registry: rows-only selectors still apply, catalog ones skip.
	if err := runReap(cliCtx(), &out, s, nil, proof.Arm(true, false), nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	due, _ := s.Due(c)
	// Store order is unspecified (readers sort); the set is the contract.
	got := map[string]bool{}
	for _, r := range due {
		got[r.Repo+":"+r.Tag] = true
	}
	if len(due) != 2 || !got["apple:10m"] || !got["zebra:10m"] {
		t.Errorf("due = %+v, want {apple:10m zebra:10m}", due)
	}
	// evaluate itself sorts: the order contract lives there, not in Due.
	marked, err := keeper.EvaluatePolicies(cliCtx(), s, nil, cliNow, nil)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(marked) != 2 || marked[0].Repo != "apple" || marked[1].Repo != "zebra" {
		t.Errorf("evaluate = %+v, want sorted [apple zebra]", marked)
	}
}

// The reap command runs dry against a file store with no registry:
// policy selection ("all" vs one name) is command plumbing the
// runReap tests never touch — the registry is dead, so catalog
// reads skip and the plan stays empty. If this fails, `reap
// <policy>` names a policy the command never passes down.
func TestReapCommandSelectsPolicy(t *testing.T) {
	clearStoreEnv(t)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	for _, args := range [][]string{{"reap"}, {"reap", "ttl"}} {
		RootCmd.SetArgs(args)
		defer RootCmd.SetArgs(nil)
		if err := RootCmd.Execute(); err != nil {
			t.Errorf("kpr %v on empty file store: %v, want a clean dry plan", args, err)
		}
	}
}

// reap without a registry client still applies the rows-only policies:
// a missing catalog skips catalog selectors, never the whole pass. If
// this fails, reap is all-or-nothing on registry reachability.
func TestReapNilRegistryMarksRowsOnly(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, nil, proof.Arm(true, false), nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap with nil registry: %v", err)
	}
	if due, _ := s.Due(context.Background()); len(due) != 1 {
		t.Errorf("nil-registry reap marked %d rows, want 1 (TTL needs no catalog)", len(due))
	}
}

// A 500 catalog for a repo skips its untagged selector but still marks
// its expired rows: one sick endpoint must not blind the whole pass,
// and must not mark rows it cannot see. If this fails, a catalog blip
// either starves reap or mass-marks the repo.
func TestReapSkipsRepoOnCatalogFailure(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "old", Tag: "v1",
		Digest: "sha256:o", PushedAt: cliNow.Add(-200 * 24 * time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, registry.NewClient(broken.URL), proof.Arm(true, false), nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	due, _ := s.Due(c)
	if len(due) != 1 || due[0].Tag != "10m" {
		t.Errorf("due = %+v, want only the expired row (failed catalog skips untagged)", due)
	}
}

// The dry-run plan renders every candidate line, in order: with a
// single row, an early return after the first print would pass unnoticed.
// If this fails, multi-row plans silently truncate to the first row.
func TestReapDryRunRendersAllRows(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "aardvark", Tag: "5m",
		Digest: "sha256:z", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), nil, nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	body := out.String()
	aardvark := strings.Index(body, "aardvark:5m — ttl:5m0s elapsed\n")
	scratch := strings.Index(body, "scratch:10m — ttl:10m0s elapsed\n")
	trailer := strings.Index(body, "(dry-run: nothing marked")
	if aardvark < 0 || scratch < 0 || trailer < 0 {
		t.Fatalf("dry-run missing rows or trailer:\n%s", body)
	}
	if aardvark >= scratch || scratch >= trailer {
		t.Errorf("dry-run rows out of order or trailer misplaced:\n%s", body)
	}
}

// The dry-run plan renders the full candidate line (identity + reason):
// approval tooling reads this text, so a reformatted line must fail
// here, not slip past a substring check. If this fails, the plan's
// contract with its readers is unpinned.
func TestReapDryRunRendersFullLine(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), nil, nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	if !strings.Contains(out.String(), "scratch:10m — ttl:10m0s elapsed\n") {
		t.Errorf("dry-run line not exact:\n%s", out.String())
	}
}

// reap against dead state fails instead of marking nothing and
// calling it a plan: an unreadable backend is an error, not an empty
// evaluation. If this fails, outages print confident empty plans.
func TestReapOnDeadRedisFails(t *testing.T) {
	if err := runReap(cliCtx(), io.Discard, deadStore{}, liveRegistryClient(t), proof.Arm(true, false), nil, cliNow, "all"); err == nil {
		t.Error("armed reap on dead redis succeeded, want an error")
	}
	if err := runReap(cliCtx(), io.Discard, deadStore{}, liveRegistryClient(t), nil, nil, cliNow, "all"); err == nil {
		t.Error("dry-run reap on dead redis succeeded, want an error")
	}
}

// The same holds for the dry-run print: a truncated candidate list is
// not a plan. If this fails, reap's safety output can silently drop
// the very rows awaiting approval.
func TestReapDryRunSurfacesWriteError(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	if err := runReap(cliCtx(), errWriter{}, s, liveRegistryClient(t), nil, nil, cliNow, "all"); err == nil {
		t.Error("dry-run reap into broken pipe succeeded, want an error")
	}
}

// `reap` shares the one-shot arming: flag or KPR_CLI_NO_DRY_RUN,
// same wiring, same test. If this fails, reap drifted off the var
// the other one-shots answer to.
func TestReapArmingWiring(t *testing.T) {
	plain := config.NewBuilder().Build()
	if armed := proof.Arm(reapNoDryRun, plain.CLINoDryRun); !proof.Unarmed(armed) {
		t.Error("reap armed by default, want implicit dry-run")
	}
	envArmed := config.NewBuilder().WithCLINoDryRun(true).Build()
	if armed := proof.Arm(reapNoDryRun, envArmed.CLINoDryRun); proof.Unarmed(armed) {
		t.Error("KPR_CLI_NO_DRY_RUN=true left reap disarmed, want armed")
	}
	if err := reapCmd.Flags().Set("no-dry-run", "true"); err != nil {
		t.Fatalf("set --no-dry-run: %v", err)
	}
	defer func() {
		_ = reapCmd.Flags().Set("no-dry-run", "false")
	}()
	if armed := proof.Arm(reapNoDryRun, plain.CLINoDryRun); proof.Unarmed(armed) {
		t.Error("--no-dry-run parsed but reap stayed disarmed, want armed")
	}
}
