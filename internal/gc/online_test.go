package gc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/proof"
)

// stageOnlineConfig stages a registry config with the store root
// plus the online surface under test: relativeurls for the gateway
// proof, and a redis blobdescriptor section only when the test
// wants the cache miss.
func stageOnlineConfig(t *testing.T, root string, cached bool) string {
	t.Helper()
	cfg := "storage:\n  filesystem:\n    rootdirectory: " + root + "\nhttp:\n  relativeurls: true\n"
	if cached {
		cfg += "redis:\n  addr: 127.0.0.1:6379\n"
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("stage online config: %v", err)
	}
	return path
}

func loopbackEdge(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func writableProbe() Probe {
	return Probe(func(context.Context, string) (Mode, string, error) {
		return ModeWritable, "", nil
	})
}

// The cleared preflight mints both tokens and reports two oks:
// cache absent, edge proven and listening. If this fails, a clean
// online run cannot start.
func TestOnlinePreflightClears(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	cfg := stageOnlineConfig(t, root, false)
	edge, done := loopbackEdge(t)
	defer done()

	cache, fence, report, err := onlinePreflight(ctx, cfg, edge, true, nil, nil)
	if err != nil {
		t.Fatalf("cleared preflight refused: %v", err)
	}
	if cache == nil || fence == nil {
		t.Fatal("cleared preflight minted nil, want both tokens")
	}
	for _, want := range []string{"[ok] blob cache", "[ok] gateway"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

// Every miss lands in one refusal: a cached config plus no fence
// names both risks with both overrides, never one at a time. If
// this fails, the operator fixes half the run per attempt — or
// misses a risk entirely.
func TestOnlinePreflightRefusesAllMissesAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	cfg := stageOnlineConfig(t, root, true)

	_, _, report, err := onlinePreflight(ctx, cfg, "127.0.0.1:1", false, nil, nil)
	if err == nil {
		t.Fatal("doubly-missed preflight cleared, want refusal")
	}
	if report == "" {
		t.Fatal("refusal carries no checklist, want every check voiced")
	}
	for _, want := range []string{
		"[miss] blob cache", "127.0.0.1:6379", "--accept-blob-cache",
		"[miss] gateway", "--accept-unfenced",
		"refusing:",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "[ok]") {
		t.Errorf("refusal claims an ok with two misses:\n%v", err)
	}
}

// Accepted risks read [accepted], naming what was waived —
// never [ok]. If this fails, the report claims a fence that was
// only waived.
func TestOnlinePreflightVoicesAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	cfg := stageOnlineConfig(t, root, true)
	accept := proof.Force(proof.Arm(true, false), true)

	_, _, report, err := onlinePreflight(ctx, cfg, "127.0.0.1:1", false, accept, accept)
	if err != nil {
		t.Fatalf("doubly-accepted preflight refused: %v", err)
	}
	for _, want := range []string{"[accepted] blob cache", "127.0.0.1:6379", "[accepted] gateway", "127.0.0.1:1"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	for _, gone := range []string{"[ok]", "[miss]"} {
		if strings.Contains(report, gone) {
			t.Errorf("report claims %q on a fully-waived run:\n%s", gone, report)
		}
	}
}

// An override the world does not need reads [ok]: acceptance
// never downgrades genuine evidence. If this fails, passing a
// redundant flag slanders a proven edge.
func TestOnlinePreflightRedundantAcceptStaysOk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	cfg := stageOnlineConfig(t, root, false)
	edge, done := loopbackEdge(t)
	defer done()
	accept := proof.Force(proof.Arm(true, false), true)

	_, _, report, err := onlinePreflight(ctx, cfg, edge, true, accept, accept)
	if err != nil {
		t.Fatalf("proven preflight with redundant accepts refused: %v", err)
	}
	for _, gone := range []string{"[accepted]", "[miss]"} {
		if strings.Contains(report, gone) {
			t.Errorf("report claims %q on a proven run:\n%s", gone, report)
		}
	}
}

// One acceptance clears exactly its risk: the cached registry
// with --accept-blob-cache still refuses the silent gateway. If
// this fails, overrides leak across risks.
func TestOnlinePreflightAcceptsPerRisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	cfg := stageOnlineConfig(t, root, true)

	cacheAccept := proof.Force(proof.Arm(true, false), true)
	_, _, _, err := onlinePreflight(ctx, cfg, "127.0.0.1:1", false, cacheAccept, nil)
	if err == nil {
		t.Fatal("cache-only acceptance cleared the gateway, want refusal")
	} else if !strings.Contains(err.Error(), "[miss] gateway") || strings.Contains(err.Error(), "[miss] blob cache") {
		t.Errorf("refusal misattributes the miss:\n%v", err)
	}
	// The footer overrides exactly the miss: a gateway-only miss
	// advises --accept-unfenced, never --accept-blob-cache. The
	// [accepted] cache line names its own flag above the footer —
	// only the footer (past "refusing:") is asserted here. If this
	// fails, the operator overrides a risk they do not have.
	footer := err.Error()[strings.Index(err.Error(), "refusing:"):]
	if !strings.Contains(footer, "--accept-unfenced") || strings.Contains(footer, "--accept-blob-cache") {
		t.Errorf("footer overrides the wrong risk:\n%v", err)
	}
}

// Mirror case: a cache-only miss (proven edge listening, lease
// configured) advises only --accept-blob-cache. If this fails,
// the operator is told to drop the fence for a cache problem.
func TestOnlinePreflightFooterOverridesCacheOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	cfg := stageOnlineConfig(t, root, true)
	edge, done := loopbackEdge(t)
	defer done()

	_, _, _, err := onlinePreflight(ctx, cfg, edge, true, nil, nil)
	if err == nil {
		t.Fatal("cache-missed preflight cleared, want refusal")
	}
	if !strings.Contains(err.Error(), "[miss] blob cache") || strings.Contains(err.Error(), "[miss] gateway") {
		t.Errorf("refusal misattributes the miss:\n%v", err)
	}
	if !strings.Contains(err.Error(), "--accept-blob-cache") || strings.Contains(err.Error(), "--accept-unfenced") {
		t.Errorf("footer overrides the wrong risk:\n%v", err)
	}
}

// A dry-run preview on a serving registry prints the preflight
// checklist and previews on: dry-run refuses nothing (it deletes
// nothing), but the operator still sees every miss an armed run
// would demand. If this fails, previews hide the clearance state.
func TestRunWritableDryRunPrintsPreflight(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	stagePairedGen(t, s, root)
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, writableProbe(), s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}}, nil, nil)
	if err != nil {
		t.Fatalf("writable preview: %v", err)
	}
	for _, want := range []string{"gc online preflight", "[ok] blob cache", "[miss] gateway", "dry-run mode, nothing will be deleted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("preview missing %q:\n%s", want, out.String())
		}
	}
	if len(collected) != 1 {
		t.Errorf("preview collected %d times, want 1 (previews still preview)", len(collected))
	}
}

// --force no longer opens the writable collect: clock skew,
// restored lineage, post-run flips are its remaining jobs, and the
// online gate does not consult it. If this fails, force re-entered
// the gate it was retired from.
func TestRunForceAloneOpensNothingOnline(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, writableProbe(), s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: false, Force: true, Report: func(Event) {}}, nil, nil)
	if err == nil {
		t.Fatal("forced uncleared run succeeded, want the preflight refusal")
	} else if !strings.Contains(err.Error(), "gateway") {
		t.Errorf("refusal is not the preflight:\n%v", err)
	}
	if len(collected) != 0 {
		t.Error("refused run reached the collector")
	}
}

// The online happy path, end to end at the use-case seam: serving
// registry, cache off, proven edge listening, fence configured —
// armed collects, and the HOLD engages around the collect. If this
// fails, a clean online run cannot reach its delete.
func TestRunOnlineCollectsUnderFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, root, s := stageProvenRun(t)
	onlineCfg := stageOnlineConfig(t, root, false)
	stagePairedGen(t, s, root)
	edge, done := loopbackEdge(t)
	defer done()

	var events []string
	fence := stubFencer{events: &events}
	var collected [][]string
	var out strings.Builder
	err := Run(ctx, &out, writableProbe(), s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", onlineCfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{EdgeAddr: edge, Fence: fence, Report: func(Event) {}}, nil, nil)
	if err != nil {
		t.Fatalf("cleared online run: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("collector ran %d times, want 1 (armed collect)", len(collected))
	}
	hold, release := false, false
	for _, e := range events {
		hold = hold || e == "hold"
		release = release || e == "release"
	}
	if !hold || !release {
		t.Errorf("fence events = %v, want hold around the collect and release after", events)
	}
}

// A refused preflight checklist that cannot print fails the
// preview: the dry-run report is the whole point of previewing an
// uncleared run. If this fails, a preview of a risky run prints
// nothing and claims nothing.
func TestRunOnlineRefusalReportWriteFailureSurfaces(t *testing.T) {
	_, root, s := stageProvenRun(t)
	var events []string
	fence := stubFencer{events: &events}
	w := errWriter{errTestStoreDown}
	err := Run(context.Background(), w, writableProbe(), s, okCollector(nil), fileAPI{root},
		"http://registry:5000", stageOnlineConfig(t, root, false), "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Fence: fence, Report: func(Event) {}}, nil, nil)
	if err == nil {
		t.Fatal("uncleared preview with dead output succeeded, want failure")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("failure names no output cause: %v", err)
	}
}

// A cleared preflight checklist that cannot print fails the run
// before the mint: no proof without the printed clearance. If this
// fails, a cleared run mints a generation the operator never saw
// cleared.
func TestRunOnlineClearReportWriteFailureSurfaces(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, root, s := stageProvenRun(t)
	onlineCfg := stageOnlineConfig(t, root, false)
	stagePairedGen(t, s, root)
	edge, done := loopbackEdge(t)
	defer done()

	var events []string
	fence := stubFencer{events: &events}
	w := errWriter{errTestStoreDown}
	err := Run(ctx, w, writableProbe(), s, okCollector(nil), fileAPI{root},
		"http://registry:5000", onlineCfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{EdgeAddr: edge, Fence: fence, Report: func(Event) {}}, nil, nil)
	if err == nil {
		t.Fatal("cleared online run with dead output succeeded, want failure")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("failure names no output cause: %v", err)
	}
}
