package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// captureLog redirects the standard logger into a buffer for the test's
// scope: error branches that only log are otherwise invisible to
// assertions.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// waitShutdown waits for the server's shutdown watcher to finish, so log
// assertions after it observe every shutdown write. Without this edge, a
// watcher write can land after Start has already returned and a single
// read would miss it nondeterministically.
func waitShutdown(t *testing.T, s *Server) {
	t.Helper()
	select {
	case <-s.shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown watcher did not finish")
	}
}

// errWrite is the sentinel write failure errWriter always returns.
var errWrite = errors.New("nope")

// testConfig installs a hermetic config with loopback display values and
// returns the restore via t.Cleanup.
func testConfig(t *testing.T) {
	t.Helper()
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithManagementHost("127.0.0.1").
		WithManagementPort(19300).
		WithAppHost("127.0.0.1").
		WithAppPort(18080).
		WithRedisAddr("redis:6379").
		Build()))
}

// An operator opening the console must see a dashboard naming the running
// build and its effective bind addresses — not a template error, and not
// stale environment reads. If this fails, the console misreports what it
// is and where it listens.
func TestIndexHandler(t *testing.T) {
	testConfig(t)
	logs := captureLog(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	IndexHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{config.Version, "127.0.0.1", "19300", "18080"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard does not mention %q", want)
		}
	}
	// A clean render logs nothing: a success that logs a template error
	// means the error branch fires when it shouldn't.
	if out := logs.String(); strings.Contains(out, "Error") {
		t.Errorf("clean render logged errors: %q", out)
	}
}

// Anything but the exact root path must 404: the dashboard handler owns
// "/" and nothing else. If this fails, unknown console paths render the
// dashboard instead of a proper 404.
func TestIndexHandlerNotFound(t *testing.T) {
	testConfig(t)
	req := httptest.NewRequest(http.MethodGet, "/metrics/../x", nil)
	rr := httptest.NewRecorder()
	IndexHandler(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

// Scrapers and operators polling /metrics must get the running build's
// identity plus the effective KPR_* configuration as JSON. If this
// fails, monitoring can't tell which build or which bindings it is
// looking at.
func TestMetricsHandler(t *testing.T) {
	testConfig(t)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	MetricsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body metricsData
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version != config.Version || body.Commit != config.Commit {
		t.Errorf("identity = %q/%q", body.Version, body.Commit)
	}
	if body.Environment["KPR_APP_PORT"] != "18080" || body.Environment["KPR_REDIS_ADDR"] != "redis:6379" {
		t.Errorf("environment = %v", body.Environment)
	}
}

// The health endpoint is what compose and the container HEALTHCHECK poll:
// it must answer 200 with a fixed body and never depend on config state.
// If this fails, orchestration restarts a healthy process (or keeps a
// dead one).
func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	HealthHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "OK\n" {
		t.Errorf("body = %q, want %q", rr.Body.String(), "OK\n")
	}
}

// errWriter is a ResponseWriter whose Write always fails.
type errWriter struct{ header http.Header }

func (w errWriter) Header() http.Header       { return w.header }
func (w errWriter) Write([]byte) (int, error) { return 0, errWrite }
func (w errWriter) WriteHeader(int)           {}

// A broken connection mid-response must not panic the console handlers:
// the error is logged and the handler returns. If this fails, one wedged
// scraper can take down the console's serving goroutine with it.
func TestHandlersTolerateWriteError(t *testing.T) {
	testConfig(t)
	logs := captureLog(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	MetricsHandler(errWriter{header: http.Header{}}, req)
	HealthHandler(errWriter{header: http.Header{}}, req)
	if n := strings.Count(logs.String(), "Error"); n != 2 {
		t.Errorf("logged %d write errors, want 2 (metrics + health)", n)
	}
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
	deadline := time.Now().Add(10 * time.Second)
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

// loopbackListener opens an ephemeral loopback port for a server test.
func loopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// A started console must answer the dashboard, metrics, and health
// endpoints — then shut down cleanly on context cancel. If this fails,
// `serve` comes up but the console doesn't actually answer, or it hangs
// on shutdown instead of draining.
func TestServerStartServesAndStopsGracefully(t *testing.T) {
	testConfig(t)
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/health")

	// Healthy serving logs nothing: a success that logs an error means
	// the error branches lie.
	logs := captureLog(t)
	for path, want := range map[string]int{
		"/":        http.StatusOK,
		"/metrics": http.StatusOK,
		"/health":  http.StatusOK,
	} {
		resp, err := testClient.Get(base + path) //nolint:gosec,noctx // test-only loopback
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	if out := logs.String(); strings.Contains(out, "Error") {
		t.Errorf("healthy serving logged errors: %q", out)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after cancel")
	}
	// NOTE: lowercase — the shutdown line reads "shutdown error", not
	// "Error". Matching the exact text is what kills a negated
	// error check here; a capital-E substring misses it. The watcher
	// wait is what makes the read deterministic: without it, the
	// watcher's write can land after Start has returned.
	waitShutdown(t, s)
	if out := logs.String(); strings.Contains(out, "shutdown error") {
		t.Errorf("clean shutdown logged errors: %q", out)
	}
}

// An unlistenable address must fail Start fast with an error, not block
// forever or panic. If this fails, a typo'd bind address hangs `serve`
// instead of reporting the problem.
func TestServerStartListenFailure(t *testing.T) {
	testConfig(t)
	s := &Server{Host: "127.0.0.1", Port: -1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err == nil {
		t.Error("Start with an invalid port succeeded, want an error")
	}
}

// A listener that dies underneath the server must surface as a Start
// error so the caller can react, rather than being swallowed. If this
// fails, `serve` believes a dead socket is still serving.
func TestServerStartServeError(t *testing.T) {
	testConfig(t)
	ln := loopbackListener(t)
	_ = ln.Close() // die before Serve reaches it
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err == nil {
		t.Error("Start on a closed listener succeeded, want an error")
	}
}

// Shutdown must be safe on a never-started server and must actually stop
// a running one. If this fails, shutdown paths either panic on nil or
// leave the socket bound.
func TestServerShutdown(t *testing.T) {
	testConfig(t)
	if err := (&Server{}).Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on a never-started server = %v, want nil", err)
	}

	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	waitFor(t, "http://"+ln.Addr().String()+"/health")

	logs := captureLog(t)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after Shutdown, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after Shutdown")
	}
	// A clean shutdown logs nothing: a success that logs a shutdown
	// error means the error branch fires when it shouldn't. NOTE:
	// lowercase — the line reads "shutdown error", not "Error".
	// The explicit cancel plus watcher wait make the read
	// deterministic (the deferred cancel alone would fire after the
	// assertions).
	cancel()
	waitShutdown(t, s)
	if out := logs.String(); strings.Contains(out, "shutdown error") {
		t.Errorf("clean shutdown logged errors: %q", out)
	}
}
