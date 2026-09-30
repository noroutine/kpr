package cli

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
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
		waitFor(t, "http://127.0.0.1:18232/health")
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
		waitFor(t, "http://127.0.0.1:18236/health")
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
	return []byte(`{"v":1,"gen":"019-proof","ts":"` + s.ts + `","writer":"kpr-gc"}`), nil
}

// Reachable completes the fake registry: production serves both the
// proof port and the reachability port from one client, so the stub
// does too — mirroring err, so an errored stub reddens every card.
func (s stubProofAPI) Reachable(context.Context) error {
	return s.err
}

// The sweep loop voices the proof age once at startup and only on
// fresh↔stale transitions after — a steady state logs nothing, so a
// year of fresh ticks doesn't bury the log, and a year of stale
// ticks warns once, not 500k times. If this fails, ticks spam or
// transitions go silent.
func TestLogSweepProofTransitions(t *testing.T) {
	logs := captureLog(t)
	ctx := context.Background()
	now := time.Now().UTC()
	fresh := stubProofAPI{ts: now.Format(time.RFC3339)}
	stale := stubProofAPI{ts: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)}

	if !logSweepProof(ctx, fresh, true, true) {
		t.Fatalf("startup on a fresh proof returns false")
	}
	if !logSweepProof(ctx, fresh, true, false) {
		t.Fatalf("steady fresh returns false")
	}
	if logSweepProof(ctx, stale, true, false) {
		t.Fatalf("fresh→stale returns true")
	}
	if logSweepProof(ctx, stale, false, false) {
		t.Fatalf("steady stale returns true")
	}
	if logSweepProof(ctx, nil, false, false) != true {
		t.Fatalf("nil api returns false")
	}
	if got := strings.Count(logs.String(), "Sweep: same-store proof"); got != 2 {
		t.Errorf("logged %d proof lines, want 2 (startup + one transition)", got)
	}
}
