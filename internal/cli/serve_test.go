package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// captureLog redirects the standard logger into a buffer for the test's
// scope: startup warnings and error branches that only log are otherwise
// invisible to assertions.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// testClient is a keep-alive-free HTTP client for server tests: every
// request gets a fresh connection that is closed after the response,
// so no pooled idle connection is outstanding when the test cancels the
// server. (With the default client's reuse pool, shutdown intermittently
// hung for the whole ShutdownTimeout.)
var testClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// drainAndClose fully reads and closes a test response body.
func drainAndClose(t *testing.T, resp *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// waitFor polls url until it returns 200 or the deadline passes.
func waitFor(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := testClient.Get(url) //nolint:gosec,noctx // test-only, loopback, bounded by deadline
		if err == nil {
			drainAndClose(t, resp)
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", url)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// setServeAddrs scopes the package-level bind variables to loopback test
// ports for one test. Parameter names deliberately differ from the package
// vars: a same-named parameter would shadow the package var and turn the
// assignment below into a silent self-assign (which is exactly the bug
// this comment guards against).
func setServeAddrs(t *testing.T, mgmt, app int) {
	t.Helper()
	oldHost, oldPort, oldAppHost, oldAppPort := managementHost, managementPort, appHost, appPort
	managementHost, managementPort = "127.0.0.1", mgmt
	appHost, appPort = "127.0.0.1", app
	t.Cleanup(func() {
		managementHost, managementPort = oldHost, oldPort
		appHost, appPort = oldAppHost, oldAppPort
	})
}

// A full `serve` run must bring up both servers from environment-derived
// config — warning on an unusable port value instead of dying, staying
// silent on the healthy paths — serve traffic, and shut both down cleanly
// on SIGTERM. If this fails, the production entrypoint (flag/env
// layering, both listeners, graceful shutdown) is untested wiring, or an
// error branch logs when it shouldn't (or stays silent when it should
// warn).
func TestServeRunServesAndStopsOnSigterm(t *testing.T) {
	t.Setenv("KPR_MANAGEMENT_PORT", "bogus") // exercises the port warning, not a failure
	// Silence derives the file backend, which would come up healthy
	// (and litter a kpr/ dir in the package). This case is about the
	// degraded banner, so point at a redis that is not there.
	clearStoreEnv(t)
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	setServeAddrs(t, 18231, 18232)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	waitFor(t, "http://127.0.0.1:18231/health")
	func() {
		defer func() {
			if t.Failed() {
				t.Logf("captured logs:\n%s", logs.String())
			}
		}()
		waitFor(t, "http://127.0.0.1:18232/")
		// Readiness is a 200, but a bare 200 proves the route, not
		// the app: the expected state must come back. This env has
		// no redis, so the honest answer is degraded — an "ok"
		// here would mean the handler never asked the store.
		resp, err := testClient.Get("http://127.0.0.1:18232/api/status") //nolint:gosec,noctx // test-only, loopback
		if err != nil {
			t.Fatalf("GET app /api/status: %v", err)
		}
		raw, err := io.ReadAll(resp.Body)
		drainAndClose(t, resp)
		if err != nil {
			t.Fatalf("read app /api/status: %v", err)
		}
		got := string(raw)
		if !strings.Contains(got, `"status":"degraded"`) || !strings.Contains(got, `"store":"unreachable"`) {
			t.Errorf("app status = %s, want degraded/unreachable without redis", got)
		}
	}()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after SIGTERM")
	}

	out := logs.String()
	if !strings.Contains(out, "invalid port") {
		t.Errorf("no port warning logged for KPR_MANAGEMENT_PORT=bogus:\n%s", out)
	}
	// No "<nil>" warning: a negated nil-check logs "Warning: <nil>" on the
	// healthy path, which must fail here.
	if strings.Contains(out, "<nil>") {
		t.Errorf("clean run logged a nil warning:\n%s", out)
	}
	for _, silent := range []string{"Server error", "Error shutting down"} {
		if strings.Contains(out, silent) {
			t.Errorf("clean run logged %q:\n%s", silent, out)
		}
	}
}

// A `serve` boot with redis down still serves — the keeper sections
// degrade with a warning instead of blocking startup, and the sweeper
// loop skips ticks it cannot read. If this fails, a redis outage is a
// startup outage instead of a red banner.
func TestServeRunDegradesWithoutRedis(t *testing.T) {
	t.Setenv("KPR_REDIS_ADDR", "127.0.0.1:1") // nothing answers on port 1
	setServeAddrs(t, 18235, 18236)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	waitFor(t, "http://127.0.0.1:18235/health")
	func() {
		defer func() {
			if t.Failed() {
				t.Logf("captured logs:\n%s", logs.String())
			}
		}()
		waitFor(t, "http://127.0.0.1:18236/")
	}()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after SIGTERM")
	}

	if out := logs.String(); !strings.Contains(out, "keeper sections degrade") {
		t.Errorf("no redis-degraded warning logged:\\n%s", out)
	}
}

// A server that fails to bind at startup must take `serve` down with a
// An edge bind failure reports and stops: the proven gate cannot
// listen, so the error must reach the select — a swallowed bind
// leaves serve running unfenced. If this fails, the edge goroutine
// drops real errors (or reports clean shutdowns as failures).
func TestServeRunReportsEdgeBindFailure(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:18237")
	if err != nil {
		t.Fatalf("occupy edge port: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	proven := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(proven, []byte("http:\n  relativeurls: true\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	t.Setenv(config.EnvRegistryConfig, proven)
	t.Setenv("KPR_EDGE_ADDR", "127.0.0.1:18237")
	setServeAddrs(t, 18235, 18236)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after edge bind failure")
	}
	if out := logs.String(); !strings.Contains(out, "Server error") {
		t.Errorf("no server error logged for the edge bind failure:\n%s", out)
	}
}

// logged error rather than hanging or serving half. If this fails, a
// port conflict at boot leaves the process wedged instead of reporting
// the problem.
func TestServeRunReportsStartupFailure(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:18233")
	if err != nil {
		t.Fatalf("occupy app port: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	setServeAddrs(t, 18234, 18233) // app port already bound above
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after app bind failure")
	}
	if out := logs.String(); !strings.Contains(out, "Server error") {
		t.Errorf("no server error logged for the bind failure:\n%s", out)
	}
}

// One boot voices every degraded default: a bogus app port, redis
// DB, and time method each warn and fall back, and a closed edge
// says pushes bypass the fence. If this fails, one degraded
// default boots silent and the operator never learns.
func TestServeRunWarnsEveryDegradedDefault(t *testing.T) {
	t.Setenv("KPR_APP_PORT", "bogus")
	t.Setenv("KPR_REDIS_DB", "bogus")
	t.Setenv("KPR_TIME_METHOD", "bogus")
	t.Setenv("KPR_EDGE", "false")
	setServeAddrs(t, 18241, 18242)
	logs := captureLog(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	waitFor(t, "http://127.0.0.1:18241/health")
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after SIGTERM")
	}
	for _, want := range []string{"invalid port", "invalid redis DB", "unknown time method", "edge disabled"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no %q warning in boot logs:\n%s", want, logs.String())
		}
	}
}

// The freshness voice degrades honestly: unserved, undated, and
// stale generations all count as not-fresh with the reason named —
// never fresh, never silent. If this fails, a store with no proof
// (or an ancient one) reads as recently proven.
func TestSweepProofFreshVoicesStaleness(t *testing.T) {
	ctx := context.Background()
	if _, note := sweepProofFresh(ctx, stubProofAPI{err: errors.New("down")}); !strings.Contains(note, "unserved") {
		t.Errorf("unserved note = %q, want unserved named", note)
	}
	if _, note := sweepProofFresh(ctx, stubProofAPI{ts: "not-a-time"}); !strings.Contains(note, "unreadable") {
		t.Errorf("undated note = %q, want unreadable named", note)
	}
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if fresh, note := sweepProofFresh(ctx, stubProofAPI{ts: old}); fresh || !strings.Contains(note, "stale") {
		t.Errorf("ancient proof fresh=%v note=%q, want stale", fresh, note)
	}
	// The staleness window is 7 days, not 31 hours: a 3-day-old
	// proof is fresh, an 8-day-old one stale. If this fails, the
	// "recently proven" window moved without the docs.
	threeDays := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	if fresh, _ := sweepProofFresh(ctx, stubProofAPI{ts: threeDays}); !fresh {
		t.Error("3-day-old proof reads stale, want fresh inside the 7-day window")
	}
	eightDays := time.Now().Add(-192 * time.Hour).UTC().Format(time.RFC3339)
	if fresh, _ := sweepProofFresh(ctx, stubProofAPI{ts: eightDays}); fresh {
		t.Error("8-day-old proof reads fresh, want stale past the 7-day window")
	}
	recent := time.Now().UTC().Format(time.RFC3339)
	if fresh, note := sweepProofFresh(ctx, stubProofAPI{ts: recent}); !fresh || !strings.Contains(note, "fresh") {
		t.Errorf("recent proof fresh=%v note=%q, want fresh", fresh, note)
	}
}

// No registry means no proof line and no crash: the async
// logProofAge fires with whatever serve has, nil included. If this
// fails, a nil client panics the boot path instead of staying
// silent.
func TestLogProofAgeNilIsSilent(t *testing.T) {
	logs := captureLog(t)
	logProofAge(context.Background(), nil)
	if logs.String() != "" {
		t.Errorf("nil proof logged %q, want silence", logs.String())
	}
}

// The root command must handle --help without exiting: Execute's success
// path returns instead of calling os.Exit. If this fails, even asking for
// help kills the process with a nonzero status.
func TestExecuteHelp(t *testing.T) {
	RootCmd.SetArgs([]string{"--help"})
	defer RootCmd.SetArgs(nil)
	Execute()
}

// stubProofAPI serves one canned generation behind the sentinel.API
// port: manifest with a config digest, payload blob behind it.
type stubProofAPI struct {
	ts  string
	id  string
	err error
}

func (s stubProofAPI) GetManifest(context.Context, string, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc"}}`), nil
}

func (s stubProofAPI) GetBlob(context.Context, string, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	pay := `{"v":1,"gen":"019-proof","ts":"` + s.ts + `","writer":"kpr-gc"`
	if s.id != "" {
		pay += `,"id":"` + s.id + `"`
	}
	return []byte(pay + `}`), nil
}

// Reachable completes the fake registry: production serves both the
// proof port and the reachability port from one client, so the stub
// does too — mirroring err, so an errored stub reddens every card.
func (s stubProofAPI) Reachable(context.Context) error {
	return s.err
}

// The edge assembly pins the HOLD lease dir to the backend — the
// file store's dir, nothing on redis — and relays proof refusal as
// nils for serve's loud skip. If this fails, HOLD leases land in the
// wrong dir, or serve boots an unfenced edge thinking it proved one.
func TestAssembleEdgeHoldDirFollowsBackend(t *testing.T) {
	proven := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(proven, []byte("http:\n  relativeurls: true\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	cfg := config.NewBuilder().WithRegistryURL("http://127.0.0.1:9").Build()
	fileGate, _, err := assembleEdge(cfg, "file", "/state", store.NewMemStore(), proven, nil)
	if err != nil {
		t.Fatalf("assembleEdge(file): %v", err)
	}
	if fileGate.Dir != "/state" {
		t.Errorf("file gate dir = %q, want the store dir", fileGate.Dir)
	}
	redisGate, _, err := assembleEdge(cfg, "redis", "", store.NewMemStore(), proven, nil)
	if err != nil {
		t.Fatalf("assembleEdge(redis): %v", err)
	}
	if redisGate.Dir != "" {
		t.Errorf("redis gate dir = %q, want empty (no lease file)", redisGate.Dir)
	}
	missing := filepath.Join(t.TempDir(), "absent.yml")
	gate, h, err := assembleEdge(cfg, "file", "/state", store.NewMemStore(), missing, nil)
	if err == nil {
		t.Error("assembleEdge(missing) error = nil, want the proof refusal")
	}
	if gate != nil || h != nil {
		t.Error("assembleEdge(missing) built a gate, want nothing")
	}
}

// serve runs no automatic passes: booting the servers must leave the
// store's pass record untouched — no startup sweep, no tick. Passes
// happen only when asked (`kpr sweep`). If this fails,
// kpr grew a scheduler again.
func TestServeRunsNoAutomaticPasses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KPR_STORE", "file")
	t.Setenv("KPR_STORE_DIR", dir)
	setServeAddrs(t, 18241, 18242)

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveCmd.Run(serveCmd, nil)
	}()

	waitFor(t, "http://127.0.0.1:18241/health")
	waitFor(t, "http://127.0.0.1:18242/api/status")
	// Any startup pass would have landed by now; a tick would need
	// a loop that no longer exists.
	time.Sleep(300 * time.Millisecond)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after SIGTERM")
	}

	st := store.NewFileStore(dir)
	if cur, err := st.GetCurrent(context.Background()); err != nil {
		t.Fatalf("GetCurrent: %v", err)
	} else if cur.Trigger != "" {
		t.Errorf("serve boot recorded a %q pass, want the store untouched", cur.Trigger)
	}
	if acts, _ := st.Activity(context.Background()); len(acts) != 0 {
		t.Errorf("serve boot recorded %d outcomes, want none", len(acts))
	}
}
