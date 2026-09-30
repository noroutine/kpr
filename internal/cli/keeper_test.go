package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
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

// status is the ssh-and-scripts banner: redis and registry reachability,
// sweeper arming, and counters from tracked state. If this fails,
// operators cannot tell a healthy keeper from a blind one.
func TestStatusRendersBannerAndCounters(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), liveRegistryClient(t), false); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	// Exact line: "unreachable" contains "reachable", so a bare
	// substring check would pass on a red banner.
	for _, want := range []string{"dry-run", "registry: reachable\n", "tracked: 2", "due: 1", "performed: 1", "planned: 1", "failed: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
}

// A down registry reddens the banner but still reports: status is a
// reader, not a health gate. Redis down, in contrast, fails fast —
// without state every number would be a lie. If this fails, status
// either hides a down registry or invents numbers with no backend.
func TestStatusDegradesAndFailsFast(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), registry.NewClient("http://127.0.0.1:1"), false); err != nil {
		t.Fatalf("registry down must not fail status: %v", err)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("status hides dead registry:\n%s", out.String())
	}
	if err := runStatus(cliCtx(), io.Discard, errStore{}, liveRegistryClient(t), false); err == nil {
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

// sweep POSTs the console trigger and prints the pass summary. When
// the console cannot be reached it degrades to the tick backstop with
// the due count — never a bare connection error. If this fails, the
// puppeteer either cannot trigger or hides the backstop.
func TestSweepTriggersAndDegrades(t *testing.T) {
	sum := sweep.Summary{PassID: "p1", Trigger: "POST", Performed: 2, Failed: 1,
		Failures: []string{"app:v1: 500"}}
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sweep" || r.Method != http.MethodPost {
			t.Errorf("trigger = %s %s, want POST /api/sweep", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(sum)
	}))
	defer console.Close()

	var out bytes.Buffer
	if err := runSweep(cliCtx(), &out, cliStore(), console.URL); err != nil {
		t.Fatalf("runSweep: %v", err)
	}
	if !strings.Contains(out.String(), "2 performed") || !strings.Contains(out.String(), "1 failed") {
		t.Errorf("summary missing counts:\n%s", out.String())
	}

	var dout bytes.Buffer
	if err := runSweep(cliCtx(), &dout, cliStore(), "http://127.0.0.1:1"); err != nil {
		t.Fatalf("unreachable console must degrade, not fail: %v", err)
	}
	for _, want := range []string{"1 rows due", "not reached", "next tick"} {
		if !strings.Contains(dout.String(), want) {
			t.Errorf("degraded sweep missing %q:\n%s", want, dout.String())
		}
	}
}

// Every keeper command fails fast naming redis when there is no
// state: the cobra wrappers are thin, but their one branch (refuse
// without a backend) must hold. If this fails, a command invents
// numbers with redis down.
func TestKeeperCommandsRefuseWithoutRedis(t *testing.T) {
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	for _, args := range [][]string{
		{"status"}, {"plan"}, {"reap"}, {"sweep"},
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

// errStore fails every read: the redis-down stand-in.
type errStore struct{ store.Store }

func (errStore) Ping(context.Context) error { return errors.New("redis: connection refused") }
func (errStore) All(context.Context) ([]policy.Row, error) {
	return nil, errors.New("redis: connection refused")
}
func (errStore) Due(context.Context) ([]policy.Row, error) {
	return nil, errors.New("redis: connection refused")
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

// The sweep trigger URL comes from the resolved console binding, with
// IPv6 hosts bracketed: an unbracketed :: would POST nowhere. If this
// fails, `sweep` calls the wrong console (or a malformed URL).
func TestConsoleURLBracketsIPv6(t *testing.T) {
	u := consoleURL(config.NewBuilder().WithManagementHost("::").WithManagementPort(9300).Build())
	if u != "http://[::]:9300" {
		t.Errorf("consoleURL = %q, want bracketed dual-stack host", u)
	}
}

// plan against dead state fails naming redis instead of printing an
// empty plan: "nothing due" must mean empty, never "unreadable". If
// this fails, an outage renders as a clean bill of health.
func TestPlanOnDeadRedisFails(t *testing.T) {
	if err := runPlan(cliCtx(), io.Discard, errStore{}, false); err == nil {
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
	if err := runStatus(cliCtx(), &out, cliStore(), nil, false); err != nil {
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

// A non-200 trigger answers the backstop, not a decode error: the tick
// picks marked rows up regardless. A 200 with garbage likewise
// degrades instead of crashing the watch. If this fails, a sick
// console turns the puppeteer into a stack trace.
func TestSweepDegradesOnBadTrigger(t *testing.T) {
	for name, status := range map[string]struct {
		code int
		body string
	}{
		"server error": {http.StatusInternalServerError, "boom"},
		"garbage":      {http.StatusOK, "{nope"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status.code)
			_, _ = w.Write([]byte(status.body))
		}))
		var out bytes.Buffer
		if err := runSweep(cliCtx(), &out, cliStore(), srv.URL); err != nil {
			t.Errorf("%s: degrading failed: %v", name, err)
		}
		if !strings.Contains(out.String(), "next tick") {
			t.Errorf("%s: no backstop:\n%s", name, out.String())
		}
		srv.Close()
	}
}

// sweep with no console and no state errors naming redis: both paths
// down is a failure, not a backstop message over invented numbers. If
// this fails, a double outage reports a confident count of nothing.
func TestSweepDoubleOutageFails(t *testing.T) {
	if err := runSweep(cliCtx(), io.Discard, errStore{}, "http://127.0.0.1:1"); err == nil {
		t.Error("sweep with console and redis down succeeded, want an error")
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
	sum := sweep.Summary{PassID: "p1", Trigger: "POST", Performed: 1,
		Failures: []string{"app:v1: 500"}}
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(sum)
	}))
	defer console.Close()
	if err := runSweep(cliCtx(), &failAfterWriter{}, cliStore(), console.URL); err == nil {
		t.Error("sweep into failing pipe succeeded, want an error")
	}
	if err := runSweep(cliCtx(), &failAfterWriter{n: 1}, cliStore(), console.URL); err == nil {
		t.Error("sweep failing on the failures line succeeded, want an error")
	}
}

// reap against dead state fails instead of marking nothing and
// calling it a plan: an unreadable backend is an error, not an empty
// evaluation. If this fails, outages print confident empty plans.
func TestReapOnDeadRedisFails(t *testing.T) {
	if err := runReap(cliCtx(), io.Discard, errStore{}, liveRegistryClient(t), true, nil, cliNow, "all"); err == nil {
		t.Error("armed reap on dead redis succeeded, want an error")
	}
	if err := runReap(cliCtx(), io.Discard, errStore{}, liveRegistryClient(t), false, nil, cliNow, "all"); err == nil {
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

// The serve loop fires sweep-on-start immediately (no full-interval
// wait after a restart) and keeps ticking until shutdown: exactly the
// two server-side triggers, no queue. If this fails, restarts sleep
// through due rows or the loop outlives serve.
func TestSweeperLoopStartupAndTick(t *testing.T) {
	s := store.NewMemStore()
	_ = s.Record(context.Background(), policy.Row{Repo: "scratch", Tag: "10m",
		Digest: "sha256:a", PushedAt: cliNow.Add(-time.Hour),
		Due: true, Reason: "ttl:10m elapsed"})
	sw := &sweep.Sweeper{Store: s, Registry: liveRegistryClient(t), DryRun: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); startSweeperLoop(ctx, sw, 20*time.Millisecond) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper loop outlived cancel")
	}
	cur, err := s.GetCurrent(context.Background())
	if err != nil {
		t.Fatalf("GetCurrent: %v", err)
	}
	if cur.Stage != sweep.StageDone || cur.Trigger == "" {
		t.Errorf("current = %+v, want a completed pass on record", cur)
	}
	acts, _ := s.Activity(context.Background())
	if len(acts) == 0 {
		t.Error("loop ran no passes, want startup + ticks recorded")
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
