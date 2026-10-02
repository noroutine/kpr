package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var cliNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func cliCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = cancel
	return c
}

func cliStore() *store.MemStore {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: cliNow.Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "latest", Digest: "sha256:b",
		PushedAt: cliNow.Add(-time.Hour)})
	_ = s.PushActivity(c, store.Outcome{Repo: "scratch", Tag: "10m",
		Reason: "ttl:10m elapsed", Outcome: "deleted", At: cliNow})
	_ = s.PushActivity(c, store.Outcome{Repo: "scratch", Tag: "9m",
		Reason: "ttl:9m elapsed", Outcome: "planned", At: cliNow})
	_ = s.PushActivity(c, store.Outcome{Repo: "app", Tag: "v1",
		Reason: "keep-n:exceeds 10", Outcome: "failed", At: cliNow})
	return s
}

func liveRegistryClient(t *testing.T) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return registry.NewClient(srv.URL)
}

// status is the ssh-and-scripts banner: redis and registry reachability
// plus counters from tracked state. If this fails, operators cannot
// tell a healthy keeper from a blind one.
func TestStatusRendersBannerAndCounters(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), liveRegistryClient(t)); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	// Exact line: "unreachable" contains "reachable", so a bare
	// substring check would pass on a red banner.
	for _, want := range []string{"registry: reachable\n", "tracked: 2", "due: 1", "performed: 1", "planned: 1", "failed: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
	// Store facts live under `store status` now — the banner must
	// not duplicate them.
	for _, gone := range []string{"store-lock:", "proof:", "identity:"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("status leaks %q, owned by store status:\n%s", gone, out.String())
		}
	}
	// Arming is per-invocation, not a posture: status owns none of
	// it, so it advertises neither armed nor dry-run. If this
	// fails, the dead serve loop's wording crept back into a banner
	// that reports on nothing armable.
	for _, gone := range []string{"armed", "dry-run", "sweeper:"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("status advertises %q (arming the banner doesn't own):\n%s", gone, out.String())
		}
	}
	if strings.Contains(out.String(), "redis: reachable") {
		t.Errorf("status names redis on a mem store:\n%s", out.String())
	}
}

// A down registry reddens the banner but still reports: status is a
// reader, not a health gate. Redis down, in contrast, fails fast —
// without state every number would be a lie. If this fails, status
// either hides a down registry or invents numbers with no backend.
func TestStatusDegradesAndFailsFast(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), registry.NewClient("http://127.0.0.1:1")); err != nil {
		t.Fatalf("registry down must not fail status: %v", err)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("status hides dead registry:\n%s", out.String())
	}
	if err := runStatus(cliCtx(), io.Discard, deadStore{}, liveRegistryClient(t)); err == nil {
		t.Error("redis down succeeded, want a fast clear error")
	} else if !strings.Contains(err.Error(), "redis") {
		t.Errorf("error = %q, want it to name redis", err.Error())
	}
}

// plan prints pending candidates with reasons, optionally as JSON for
// piping into other tools. If this fails, the dry-run view and the
// reap view diverge — or scripts cannot consume the plan.
func TestPlanListsDueWithReasons(t *testing.T) {
	var out bytes.Buffer
	if err := runPlan(cliCtx(), &out, cliStore(), false); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if !strings.Contains(out.String(), "scratch:10m") || !strings.Contains(out.String(), "ttl:10m elapsed") {
		t.Errorf("plan missing candidate or reason:\n%s", out.String())
	}
	if strings.Contains(out.String(), "app:latest") {
		t.Errorf("plan lists unmarked :latest:\n%s", out.String())
	}

	var jout bytes.Buffer
	if err := runPlan(cliCtx(), &jout, cliStore(), true); err != nil {
		t.Fatalf("runPlan json: %v", err)
	}
	var decoded []map[string]string
	if err := json.Unmarshal(jout.Bytes(), &decoded); err != nil {
		t.Fatalf("plan --json is not JSON: %v\n%s", err, jout.String())
	}
	if len(decoded) != 1 || decoded[0]["reason"] == "" {
		t.Errorf("plan --json = %v, want one candidate with a reason", decoded)
	}
}

// plan with several due rows renders them sorted by repo then tag: a
// swapped comparator must fail here. If this fails, the plan order
// contract is unpinned.
func TestPlanRendersRowsSorted(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "zebra", Tag: "v1", Digest: "sha256:1",
		PushedAt: cliNow, Due: true, Reason: "x"})
	_ = s.Record(c, policy.Row{Repo: "apple", Tag: "v2", Digest: "sha256:2",
		PushedAt: cliNow, Due: true, Reason: "y"})
	_ = s.Record(c, policy.Row{Repo: "apple", Tag: "v1", Digest: "sha256:3",
		PushedAt: cliNow, Due: true, Reason: "z"})
	var out bytes.Buffer
	if err := runPlan(cliCtx(), &out, s, false); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	body := out.String()
	ordered := []string{"apple:v1", "apple:v2", "zebra:v1"}
	last := -1
	for _, want := range ordered {
		at := strings.Index(body, want)
		if at <= last {
			t.Errorf("plan order broken at %q:\n%s", want, body)
			break
		}
		last = at
	}
}

// Unarmed reap only prints the plan (same source as plan): nothing is
// marked, nothing will sweep. If this fails, the default run mutates —
// the implicit-dry-run promise broken at its most important site.
func TestReapDryRunPrintsWithoutMarking(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), false, nil, cliNow, "all"); err != nil {
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
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), true, nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	due, _ := s.Due(c)
	if len(due) != 1 || due[0].Tag != "10m" {
		t.Fatalf("due = %+v, want [scratch:10m]", due)
	}
	if due[0].Reason == "" {
		t.Error("marked row carries no reason")
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
	if err := runReap(cliCtx(), &out, s, nil, true, []string{"^scratch:"}, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	if due, _ := s.Due(c); len(due) != 0 {
		t.Errorf("excluded repo lost %d rows to keep-N, want 0", len(due))
	}
}

// plan discard drops the whole plan (due marks) and says how many went.
// No dry-run: discarding previews nothing, it reports. If this fails,
// a stale plan survives its discard and the next sweep eats rows the
// operator already pardoned.
// reapStage builds one expired row, one partial (digest-less, stale)
// row, and an 11-tag pile whose oldest is keep-n's victim: each
// selective reap must mark only its own policy's rows.
func reapStage(s *store.MemStore) {
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: cliNow.Add(-time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "partial",
		PushedAt: cliNow.Add(-25 * time.Hour)})
	for i := 1; i <= 11; i++ {
		_ = s.Record(c, policy.Row{Repo: "pile", Tag: fmt.Sprintf("v%02d", i),
			Digest: "sha256:p", PushedAt: cliNow.Add(-time.Duration(12-i) * time.Minute)})
	}
}

// A named reap marks only that policy's rows: expired takes the TTL
// row, partial the digest-less stale one, keep-n the pile's oldest.
// If this fails, selective reaping leaks across policies.
func TestReapSelectivePolicy(t *testing.T) {
	for pol, want := range map[string][2]string{
		"expired": {"scratch", "10m"},
		"partial": {"scratch", "partial"},
		"keep-n":  {"pile", "v01"},
	} {
		s := store.NewMemStore()
		reapStage(s)
		var out bytes.Buffer
		if err := runReap(cliCtx(), &out, s, nil, true, nil, cliNow, pol); err != nil {
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

func dueTags(due []policy.Row) []string {
	var out []string
	for _, r := range due {
		out = append(out, r.Repo+":"+r.Tag)
	}
	return out
}

// Successive reaps accumulate in the plan until sweep or discard:
// reaping expired then partial leaves both rows due. If this fails,
// a second reap wipes the first policy's marks.
func TestReapAccumulatesAcrossPolicies(t *testing.T) {
	s := store.NewMemStore()
	reapStage(s)
	var out bytes.Buffer
	if err := runReap(cliCtx(), &out, s, nil, true, nil, cliNow, "expired"); err != nil {
		t.Fatalf("reap expired: %v", err)
	}
	if err := runReap(cliCtx(), &out, s, nil, true, nil, cliNow, "partial"); err != nil {
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
	err := runReap(cliCtx(), &out, s, nil, true, nil, cliNow, "bogus")
	if err == nil {
		t.Fatal("reap bogus succeeded, want refusal")
	}
	for _, name := range []string{"all", "expired", "partial", "untagged", "keep-n"} {
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
	if err := runReap(cliCtx(), &out, s, nil, true, nil, cliNow, "all"); err != nil {
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

// sweepStub is the registry as the direct sweeper consumes it:
// deletes plus the sentinel read port, no HTTP. The served
// generation pairs the staged store below.
type sweepStub struct {
	stubProofAPI
	outcome string
	delErr  error
}

func (s sweepStub) DeleteManifest(context.Context, string, string) (string, error) {
	return s.outcome, s.delErr
}

// pairedSweepStore stages one due row on paired, unlocked ground
// with a fresh served generation: a pass runs instead of refusing.
func pairedSweepStore(t *testing.T) (*store.MemStore, sweepStub) {
	t.Helper()
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	_ = s.SetIdentity(c, store.Identity{ID: "cli-lineage", BaselineGen: "019-proof"})
	if err := s.SetUnlocked(c, true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	stub := sweepStub{
		stubProofAPI: stubProofAPI{
			ts: time.Now().UTC().Format(time.RFC3339),
			id: "cli-lineage",
		},
		outcome: registry.OutcomeDeleted,
	}
	return s, stub
}

// sweep runs the pass in-process and prints its summary: dry-run
// plans (the row stays), armed deletes by digest (the row goes).
// No console, no POST — the sweeper lives in the CLI. If this
// fails, the pass either cannot run or hides the counts.
func TestSweepRunsDirectDryRunAndArmed(t *testing.T) {
	s, stub := pairedSweepStore(t)
	var out bytes.Buffer
	if err := runSweep(cliCtx(), &out, s, stub, false); err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	if !strings.Contains(out.String(), "0 performed") || !strings.Contains(out.String(), "1 planned") {
		t.Errorf("dry-run summary missing counts:\n%s", out.String())
	}
	if rows, _ := s.All(context.Background()); len(rows) != 1 {
		t.Errorf("dry-run kept %d rows, want the 1 planned row kept", len(rows))
	}

	as, astub := pairedSweepStore(t)
	var aout bytes.Buffer
	if err := runSweep(cliCtx(), &aout, as, astub, true); err != nil {
		t.Fatalf("armed sweep: %v", err)
	}
	if !strings.Contains(aout.String(), "1 performed") {
		t.Errorf("armed summary missing counts:\n%s", aout.String())
	}
	if rows, _ := as.All(context.Background()); len(rows) != 0 {
		t.Errorf("armed kept %d rows, want the deleted row dropped", len(rows))
	}
}

// Every keeper command fails fast naming redis when there is no
// state: the cobra wrappers are thin, but their one branch (refuse
// without a backend) must hold. If this fails, a command invents
// numbers with redis down.
func TestKeeperCommandsRefuseWithoutRedis(t *testing.T) {
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	for _, args := range [][]string{
		{"status"}, {"plan"}, {"plan", "discard"}, {"reap"}, {"reap", "expired"}, {"sweep"},
	} {
		RootCmd.SetArgs(args)
		defer RootCmd.SetArgs(nil)
		if err := RootCmd.Execute(); err == nil {
			t.Errorf("kpr %s without redis succeeded, want a fast error", args[0])
		} else if !strings.Contains(err.Error(), "redis") {
			t.Errorf("kpr %s error = %q, want it to name redis", args[0], err.Error())
		}
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
	for _, args := range [][]string{{"reap"}, {"reap", "expired"}} {
		RootCmd.SetArgs(args)
		defer RootCmd.SetArgs(nil)
		if err := RootCmd.Execute(); err != nil {
			t.Errorf("kpr %v on empty file store: %v, want a clean dry plan", args, err)
		}
	}
}

// deadStore is a store whose redis is gone: every method reports the
// outage instead of embedding a zero store.Store whose promoted
// methods nil-deref. If this fails, the outage test panics instead
// of asserting the loud report.
type deadStore struct{}

func (deadStore) outage() error { return errors.New("redis: connection refused") }

func (d deadStore) Ping(context.Context) error { return d.outage() }
func (d deadStore) Record(context.Context, policy.Row) error {
	return d.outage()
}
func (d deadStore) All(context.Context) ([]policy.Row, error) {
	return nil, d.outage()
}
func (d deadStore) Due(context.Context) ([]policy.Row, error) {
	return nil, d.outage()
}
func (d deadStore) MarkDue(context.Context, string, string, string) error {
	return d.outage()
}
func (d deadStore) ClearDue(context.Context) (int, error) {
	return 0, d.outage()
}
func (d deadStore) UnmarkDue(context.Context, string, string) (bool, error) {
	return false, d.outage()
}
func (d deadStore) Delete(context.Context, string, string) error {
	return d.outage()
}
func (d deadStore) SetCurrent(context.Context, store.Current) error {
	return d.outage()
}
func (d deadStore) GetCurrent(context.Context) (store.Current, error) {
	return store.Current{}, d.outage()
}
func (d deadStore) PushActivity(context.Context, store.Outcome) error {
	return d.outage()
}
func (d deadStore) Activity(context.Context) ([]store.Outcome, error) {
	return nil, d.outage()
}
func (d deadStore) AcquireLock(context.Context, string, time.Duration) (bool, error) {
	return false, d.outage()
}
func (d deadStore) ReleaseLock(context.Context, string) error {
	return d.outage()
}
func (d deadStore) IsUnlocked(context.Context) (bool, error) {
	return false, d.outage()
}
func (d deadStore) SetUnlocked(context.Context, bool) error {
	return d.outage()
}
func (d deadStore) GetIdentity(context.Context) (store.Identity, error) {
	return store.Identity{}, d.outage()
}
func (d deadStore) SetIdentity(context.Context, store.Identity) error {
	return d.outage()
}

// Opening state against a dead redis must fail fast naming redis —
// the operator typo'd the address and needs to know it now, not after
// a hang. If this fails, keeper commands stall or blame the wrong
// backend.
func TestOpenStoreNamesDeadRedis(t *testing.T) {
	cfg := config.NewBuilder().WithRedisAddr("127.0.0.1:1").Build()
	if _, err := OpenStore(cfg); err == nil {
		t.Error("OpenStore on dead redis succeeded, want a fast error")
	} else if !strings.Contains(err.Error(), "redis") {
		t.Errorf("error = %q, want it to name redis", err.Error())
	}
}

// plan against dead state fails naming redis instead of printing an
// empty plan: "nothing due" must mean empty, never "unreadable". If
// this fails, an outage renders as a clean bill of health.
func TestPlanOnDeadRedisFails(t *testing.T) {
	if err := runPlan(cliCtx(), io.Discard, deadStore{}, false); err == nil {
		t.Error("plan on dead redis succeeded, want an error")
	}
}

// A catalog read against a dead registry fails fast: reap treats it as
// "no catalog for this repo" downstream, but the client itself must
// report the error rather than empty tags (empty would read as "every
// row untagged"). If this fails, registry blips become mass untagging.
func TestCatalogOnDeadRegistryFails(t *testing.T) {
	if _, err := registry.NewClient("http://127.0.0.1:1").Catalog(cliCtx(), "app"); err == nil {
		t.Error("catalog on dead registry succeeded, want an error")
	}
}

// status without a registry client still reports (red banner), since
// the probe is optional — only state is mandatory. If this fails, a
// nil client panics the status path instead of reddening it.
func TestStatusNilRegistry(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), nil); err != nil {
		t.Fatalf("runStatus with nil registry: %v", err)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("status hides missing registry:\n%s", out.String())
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
	if err := runReap(cliCtx(), &out, s, nil, true, nil, cliNow, "all"); err != nil {
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
	if err := runReap(cliCtx(), &out, s, registry.NewClient(broken.URL), true, nil, cliNow, "all"); err != nil {
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
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), false, nil, cliNow, "all"); err != nil {
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
	if err := runReap(cliCtx(), &out, s, liveRegistryClient(t), false, nil, cliNow, "all"); err != nil {
		t.Fatalf("runReap: %v", err)
	}
	if !strings.Contains(out.String(), "scratch:10m — ttl:10m0s elapsed\n") {
		t.Errorf("dry-run line not exact:\n%s", out.String())
	}
}

// sweep on dead state reports the outage in the summary instead of
// failing: the pass runs nowhere, resolves nothing, and narrates
// that. If this fails, an outage prints a confident count of
// nothing.
func TestSweepOutageReportsInsteadOfFailing(t *testing.T) {
	var out bytes.Buffer
	stub := sweepStub{stubProofAPI: stubProofAPI{err: errors.New("redis: connection refused")}}
	if err := runSweep(cliCtx(), &out, deadStore{}, stub, false); err != nil {
		t.Errorf("sweep on dead state failed: %v", err)
	}
	if !strings.Contains(out.String(), "failed:") {
		t.Errorf("outage summary names no failure:\n%s", out.String())
	}
}

// failAfterWriter fails every write after n successes: the flaky-pipe
// stand-in for multi-line output.
type failAfterWriter struct {
	n     int
	calls int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.n {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}

// A pipe breaking mid-summary surfaces the error: a half-printed pass
// summary must not read as success. If this fails, truncated output
// passes silently.
func TestSweepSurfacesMidSummaryWriteError(t *testing.T) {
	s, stub := pairedSweepStore(t)
	if err := runSweep(cliCtx(), &failAfterWriter{}, s, stub, false); err == nil {
		t.Error("sweep into failing pipe succeeded, want an error")
	}
}

// An unreadable marker voices unknown, never locked: the operator
// must not revoke on a read error. If this fails, an outage reads
// as intent.
func TestLockStateUnknownOnOutage(t *testing.T) {
	if got := lockState(cliCtx(), deadStore{}); got != "unknown" {
		t.Errorf("lockState on outage = %q, want unknown", got)
	}
	if got := lockState(cliCtx(), store.NewMemStore()); got != "locked" {
		t.Errorf("fresh lockState = %q, want locked (born locked)", got)
	}
}

// The backend name voices file with its dir, redis otherwise: the
// refusal names what the operator must fix. If this fails, outages
// blame the wrong backend.
func TestStoreNamesVoiceBackend(t *testing.T) {
	dir := t.TempDir()
	if got := storeName(store.NewFileStore(dir)); got != "file store" {
		t.Errorf("storeName(file) = %q, want file store", got)
	}
	if got := storeName(store.NewMemStore()); got != "redis" {
		t.Errorf("storeName(other) = %q, want redis", got)
	}
	if got := describeStore(store.NewFileStore(dir), nil); got != "file ("+dir+")" {
		t.Errorf("describeStore(file) = %q, want file with dir", got)
	}
}

// An unreadable proof voices unproven, and an undated generation
// voices its name without an age: presence without timing is still
// presence. If this fails, outages read as proofs (or proofs hide).
func TestProofStateVoicesUnproven(t *testing.T) {
	if got := proofState(cliCtx(), nil); got != "unproven" {
		t.Errorf("proofState(nil) = %q, want unproven", got)
	}
	broken := stubProofAPI{err: errors.New("connection refused")}
	if got := proofState(cliCtx(), broken); got != "unproven" {
		t.Errorf("proofState(outage) = %q, want unproven", got)
	}
	undated := stubProofAPI{ts: "not-a-time", id: "id-1"}
	if got := proofState(cliCtx(), undated); got != "019-proof" {
		t.Errorf("proofState(undated) = %q, want the gen without age", got)
	}
}

// An empty plan says so: "nothing due" must mean empty, never
// unreadable. If this fails, clean stores print blank.
func TestPlanEmptySaysNothingDue(t *testing.T) {
	var out bytes.Buffer
	if err := runPlan(cliCtx(), &out, store.NewMemStore(), false); err != nil {
		t.Fatalf("plan on empty store: %v", err)
	}
	if got := out.String(); got != "nothing due\n" {
		t.Errorf("plan = %q, want the empty line", got)
	}
}

// Discarding against dead state fails naming redis: zero must mean
// empty, never unreadable. If this fails, an outage discards on
// paper.
func TestDiscardPlanOnOutageFails(t *testing.T) {
	if err := runDiscardPlan(cliCtx(), io.Discard, deadStore{}); err == nil {
		t.Error("discard on outage succeeded, want an error")
	}
}

// Conflicting backend env refuses with the conflict named: guessing
// state wrong is worse than not booting. If this fails, file+redis
// together pick one silently.
func TestOpenStoreRefusesConflictingBackend(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	cfg := config.NewBuilder().FromEnv().Build()
	if _, err := OpenStore(cfg); err == nil {
		t.Error("OpenStore on conflicting backend succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("refusal = %q, want the conflict named", err.Error())
	}
}

// A file backend rooted at a non-directory refuses naming the dir:
// the operator learns the path is wrong, not that redis is down.
// If this fails, a bad store dir blames redis.
func TestOpenStoreRefusesBadFileDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o644); err != nil {
		t.Fatalf("stage blocker: %v", err)
	}
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, blocker)
	cfg := config.NewBuilder().FromEnv().Build()
	if _, err := OpenStore(cfg); err == nil {
		t.Error("OpenStore on file-backed dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), blocker) {
		t.Errorf("refusal = %q, want the dir named", err.Error())
	}
}

// fakeRedis answers just enough RESP for a go-redis Ping: HELLO
// gets an empty map, PING pongs, anything else oks. A real redis is
// a test dependency nobody wants; this proves OpenStore dials and
// selects, not the wire grammar.
func fakeRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeRedisConn(c)
		}
	}()
	return ln.Addr().String()
}

func serveFakeRedisConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if !strings.HasPrefix(line, "*") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
		if err != nil {
			return
		}
		var first string
		for i := range n {
			ln, err := r.ReadString('\n')
			if err != nil {
				return
			}
			m, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "$")))
			if err != nil {
				return
			}
			buf := make([]byte, m+2)
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			if i == 0 {
				first = string(buf[:m])
			}
		}
		switch strings.ToUpper(first) {
		case "HELLO":
			_, _ = c.Write([]byte("%0\r\n"))
		case "PING":
			_, _ = c.Write([]byte("+PONG\r\n"))
			return
		default:
			_, _ = c.Write([]byte("+OK\r\n"))
		}
	}
}

// Opening redis state against an answering cache succeeds: the
// success return is a real path, not a hope. If this fails, every
// redis deploy refuses at boot.
func TestOpenStoreRedisSuccess(t *testing.T) {
	addr := fakeRedis(t)
	cfg := config.NewBuilder().WithRedisAddr(addr).Build()
	s, err := OpenStore(cfg)
	if err != nil {
		t.Fatalf("OpenStore on answering redis: %v", err)
	}
	_ = s.Close()
}

// Command tails run the real RunE against a file backend: open,
// render, close — the wiring no unit covers. Each command gets one
// hermetic pass; refusals already pin the failure paths. If any of
// these fail, the command is unwired (flags, deps, or close).
func TestCommandTailsRunAgainstFileBackend(t *testing.T) {
	dir := t.TempDir()
	srv := serveRegistry(t, t.TempDir(), false)
	defer srv.Close()
	for _, tc := range []struct {
		name   string
		target *cobra.Command
		args   []string
		want   string
	}{
		{"status", statusCmd, nil, "registry:"},
		{"plan", planCmd, nil, "nothing due"},
		{"discard", planDiscardCmd, nil, "nothing due"},
		{"sweep", sweepCmd, nil, "sweep "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runLockCmd(t, dir, srv.URL, tc.target)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("%s output lacks %q:\n%s", tc.name, tc.want, out)
			}
		})
	}
}

// Every keeper command refuses without state: one bad backend
// exercises every RunE's open failure. If this fails, a command
// invents numbers without a backend.
func TestKeeperCommandsRefuseBadBackend(t *testing.T) {
	t.Setenv(config.EnvStore, "bogus-backend")
	for _, target := range []*cobra.Command{statusCmd, planCmd, planDiscardCmd, sweepCmd, gcCmd, lockCmd} {
		var buf bytes.Buffer
		target.SetOut(&buf)
		defer target.SetOut(nil)
		target.SetContext(context.Background())
		if err := target.RunE(target, nil); err == nil {
			t.Errorf("%s on bad backend succeeded, want refusal", target.Use)
		}
	}
}

// Execute exits 1 on usage failure: the production entrypoint's
// failure mode, pinned via a child process (the parent only asserts
// the exit). If this fails, CLI misuse exits 0 and scripts proceed.
func TestExecuteExitsOneOnUsageError(t *testing.T) {
	if os.Getenv("KPR_EXEC_CHILD") == "1" {
		RootCmd.SetArgs([]string{"bogus-command"})
		Execute()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestExecuteExitsOneOnUsageError")
	cmd.Env = append(os.Environ(), "KPR_EXEC_CHILD=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("bogus command exited 0, want exit 1")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("bogus command err = %v, want exit 1", err)
	}
}

// A pipe breaking on a failure line surfaces the error too: the
// summary line already printed, so only the failures-line check
// catches it. If this fails, a truncated failure list reads as a
// clean pass.
func TestSweepSurfacesFailureLineWriteError(t *testing.T) {
	s, stub := pairedSweepStore(t)
	stub.delErr = errors.New("registry: 500")
	var ok bytes.Buffer
	if err := runSweep(cliCtx(), &ok, s, stub, true); err != nil {
		t.Fatalf("armed failing sweep: %v", err)
	}
	if !strings.Contains(ok.String(), "failed:") {
		t.Fatalf("no failure lines to break on:\n%s", ok.String())
	}
	fs, fstub := pairedSweepStore(t)
	fstub.delErr = errors.New("registry: 500")
	if err := runSweep(cliCtx(), &failAfterWriter{n: 1}, fs, fstub, true); err == nil {
		t.Error("sweep failing on the failures line succeeded, want an error")
	}
}

// The sweep command arms from its own flag or the one-shot env var —
// never the dead serve loop's. The flag binding and the env half are
// both wired here: RunE only sees sweepArmed. If this fails, `sweep`
// answers to the wrong var and the suite can't see it.
func TestSweepArmingWiring(t *testing.T) {
	plain := config.NewBuilder().Build()
	if sweepArmed(plain) {
		t.Error("sweep armed by default, want implicit dry-run")
	}
	envArmed := config.NewBuilder().WithCLINoDryRun(true).Build()
	if !sweepArmed(envArmed) {
		t.Error("KPR_CLI_NO_DRY_RUN=true left sweep disarmed, want armed")
	}
	// The cobra binding: parsing --no-dry-run must flip the same
	// global RunE reads. Reset after: the flag is process-global.
	if err := sweepCmd.Flags().Set("no-dry-run", "true"); err != nil {
		t.Fatalf("set --no-dry-run: %v", err)
	}
	defer func() {
		_ = sweepCmd.Flags().Set("no-dry-run", "false")
	}()
	if !sweepArmed(plain) {
		t.Error("--no-dry-run parsed but sweep stayed disarmed, want armed")
	}
}

// `reap` shares the one-shot arming: flag or KPR_CLI_NO_DRY_RUN,
// same wiring, same test. If this fails, reap drifted off the var
// the other one-shots answer to.
func TestReapArmingWiring(t *testing.T) {
	plain := config.NewBuilder().Build()
	if reapArmed(plain) {
		t.Error("reap armed by default, want implicit dry-run")
	}
	envArmed := config.NewBuilder().WithCLINoDryRun(true).Build()
	if !reapArmed(envArmed) {
		t.Error("KPR_CLI_NO_DRY_RUN=true left reap disarmed, want armed")
	}
	if err := reapCmd.Flags().Set("no-dry-run", "true"); err != nil {
		t.Fatalf("set --no-dry-run: %v", err)
	}
	defer func() {
		_ = reapCmd.Flags().Set("no-dry-run", "false")
	}()
	if !reapArmed(plain) {
		t.Error("--no-dry-run parsed but reap stayed disarmed, want armed")
	}
}

// describeStore voices the backend in one line: file with its dir,
// redis with addr and DB. If this fails, the store card names the
// wrong backend — the operator fixes the wrong state.
func TestDescribeStoreNamesBackend(t *testing.T) {
	cfg := config.NewBuilder().WithRedisAddr("r:6379").WithRedisDB(4).Build()
	if got := describeStore(store.NewMemStore(), cfg); got != "redis (r:6379 db 4)" {
		t.Errorf("mem store described as %q, want the redis line", got)
	}
	fs := store.NewFileStore(t.TempDir())
	if got := describeStore(fs, cfg); got != "file ("+fs.Dir()+")" {
		t.Errorf("file store described as %q, want file with dir", got)
	}
}

// reap against dead state fails instead of marking nothing and
// calling it a plan: an unreadable backend is an error, not an empty
// evaluation. If this fails, outages print confident empty plans.
func TestReapOnDeadRedisFails(t *testing.T) {
	if err := runReap(cliCtx(), io.Discard, deadStore{}, liveRegistryClient(t), true, nil, cliNow, "all"); err == nil {
		t.Error("armed reap on dead redis succeeded, want an error")
	}
	if err := runReap(cliCtx(), io.Discard, deadStore{}, liveRegistryClient(t), false, nil, cliNow, "all"); err == nil {
		t.Error("dry-run reap on dead redis succeeded, want an error")
	}
}

// errWriter fails every write: the broken-pipe stand-in.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A broken pipe must surface as an error, not a silent short plan: a
// truncated plan piped into approval tooling reads as approval-worthy.
// If this fails, output errors vanish.
func TestPlanSurfacesWriteError(t *testing.T) {
	if err := runPlan(cliCtx(), errWriter{}, cliStore(), false); err == nil {
		t.Error("plan into broken pipe succeeded, want an error")
	}
}

// The same holds for the dry-run print: a truncated candidate list is
// not a plan. If this fails, reap's safety output can silently drop
// the very rows awaiting approval.
func TestReapDryRunSurfacesWriteError(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour)})
	if err := runReap(cliCtx(), errWriter{}, s, liveRegistryClient(t), false, nil, cliNow, "all"); err == nil {
		t.Error("dry-run reap into broken pipe succeeded, want an error")
	}
}

// clearStoreEnv unsets every backend signal: derivation reads
// explicitness, and a leaked CI variable would select a backend the
// case never asked for.
func clearStoreEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvStore, "")
	t.Setenv(config.EnvStoreDir, "")
	t.Setenv(config.EnvRedisAddr, "")
}

// The backend derives from explicit signals, never resolved values
// (KPR_REDIS_ADDR carries a default that must not count):
// KPR_STORE is authoritative and must agree with backend-specific
// variables, KPR_STORE_DIR alone selects file, KPR_REDIS_ADDR alone
// selects redis, silence keeps redis defaults. If this fails, mixed
// signals boot a guessed backend or refuse a coherent one.
func TestResolveStoreBackend(t *testing.T) {
	for _, tc := range []struct {
		name            string
		env             map[string]string
		wantBackend     string
		wantDir         string
		wantErrContains string
	}{
		{"silence keeps redis", nil, "redis", "", ""},
		{"explicit redis addr", map[string]string{config.EnvRedisAddr: "r:6379"}, "redis", "", ""},
		{"explicit store redis", map[string]string{config.EnvStore: "redis"}, "redis", "", ""},
		{"explicit store file defaults dir", map[string]string{config.EnvStore: "file"}, "file", "kpr", ""},
		{"store dir alone selects file", map[string]string{config.EnvStoreDir: "/x/kpr"}, "file", "/x/kpr", ""},
		{"unknown store refuses", map[string]string{config.EnvStore: "sqlite"}, "", "", "unknown"},
		{"file plus redis addr conflicts", map[string]string{config.EnvStore: "file", config.EnvRedisAddr: "r:6379"}, "", "", "conflicts"},
		{"redis plus store dir conflicts", map[string]string{config.EnvStore: "redis", config.EnvStoreDir: "/x"}, "", "", "conflicts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearStoreEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			backend, dir, err := resolveStoreBackend()
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("resolve = (%q, %q, %v), want error containing %q", backend, dir, err, tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if backend != tc.wantBackend || dir != tc.wantDir {
				t.Errorf("resolve = (%q, %q), want (%q, %q)", backend, dir, tc.wantBackend, tc.wantDir)
			}
		})
	}
}

// KPR_STORE=file opens the file backend (fail-fast Ping like redis):
// the operator gets file state or a refusal naming the dir, never a
// silent redis. If this fails, file mode boots something else.
func TestOpenStoreFileBackend(t *testing.T) {
	clearStoreEnv(t)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	cfg := config.NewBuilder().FromEnv().Build()
	s, err := OpenStore(cfg)
	if err != nil {
		t.Fatalf("OpenStore(file): %v", err)
	}
	if _, ok := s.(*store.FileStore); !ok {
		t.Errorf("store = %T, want *store.FileStore", s)
	}
}
