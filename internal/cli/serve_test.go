package cli

import (
	"bytes"
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
		waitFor(t, "http://127.0.0.1:18232/api/hello")
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
		waitFor(t, "http://127.0.0.1:18236/api/hello")
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
