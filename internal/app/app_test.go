package app

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

	"nrtn.dev/catalyst/kpr/internal/store"
)

// captureLog redirects the standard logger into a buffer for the test's
// scope: error branches that only log (encode failures, close failures)
// are otherwise invisible to assertions.
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

// /api/status is the machine contract for expected state: ok when
// the store answers, degraded when it doesn't, disabled with no
// store at all — always 200 while serving (NOK is unreachable).
// If this fails, monitors can't tell a blind receiver from a dead
// server.
func TestStatusHandlerReportsExpectedState(t *testing.T) {
	for name, tc := range map[string]struct {
		backend       store.Store
		status, state string
	}{
		"reachable": {store.NewMemStore(), "ok", "reachable"},
		"down":      {pingDownStore{}, "degraded", "unreachable"},
		"disabled":  {nil, "ok", "disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			rr := httptest.NewRecorder()
			StatusHandler(tc.backend)(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			var body StatusResponse
			if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Status != tc.status || body.Store != tc.state {
				t.Errorf("got %s/%s, want %s/%s", body.Status, body.Store, tc.status, tc.state)
			}
		})
	}
}

// pingDownStore is a Store whose Ping always fails: the degraded
// half of the status contract without needing a dead backend.
type pingDownStore struct{ store.Store }

func (pingDownStore) Ping(context.Context) error { return errors.New("store down") }

// The embedded SPA must actually be embedded: the bundle has to resolve
// index.html at runtime, not just at compile time. If this fails, the
// binary serves a 404 for its own UI.
func TestGetStaticFSHasIndex(t *testing.T) {
	fs, err := GetStaticFS()
	if err != nil {
		t.Fatalf("GetStaticFS: %v", err)
	}
	f, err := fs.Open("index.html")
	if err != nil {
		t.Fatalf("open index.html: %v", err)
	}
	_ = f.Close()
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

// A started server must serve the SPA at /, the keeper status at
// /health, and 404 anything else (the template mock API is gone) —
// then shut down cleanly on context cancel. If this fails, `serve`
// comes up but doesn't actually serve, or it hangs on shutdown
// instead of draining.
func TestServerStartServesAndStopsGracefully(t *testing.T) {
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/")

	// Healthy serving logs nothing: a success that logs an error (or a
	// failure that stays silent) means the error branches lie. Handler
	// writes complete before the fully-read response, so one read
	// observes them all.
	logs := captureLog(t)
	for path, want := range map[string]int{
		"/":              http.StatusOK,
		"/health":        http.StatusNotFound,
		"/api/status":    http.StatusOK,
		"/api/hello":     http.StatusNotFound,
		"/api/data":      http.StatusNotFound,
		"/no-such-asset": http.StatusNotFound,
	} {
		resp, err := testClient.Get(base + path) //nolint:gosec,noctx // test-only loopback
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		drainAndClose(t, resp)
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
	if err := (&Server{}).Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on a never-started server = %v, want nil", err)
	}

	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	waitFor(t, "http://"+ln.Addr().String()+"/")

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
}

// The front page is the keeper's face: ttl.sh spirit (push, pull,
// forget — homelab wording, not ephemeral-only) and the pipeline it
// actually runs. No mock API buttons, no links section, no status
// ball: keeper state lives on the console, not the landing page. If
// this fails, the template demo is back or the ball crept back in.
func TestIndexPageIsKeeperFront(t *testing.T) {
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Start(ctx) }()
	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/")

	resp, err := testClient.Get(base + "/") //nolint:gosec,noctx // test-only loopback
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	raw, err := io.ReadAll(resp.Body)
	drainAndClose(t, resp)
	if err != nil {
		t.Fatalf("read /: %v", err)
	}
	body := string(raw)
	for _, want := range []string{"Push. Pull.", "reap", "sweep", "keeper"} {
		if !strings.Contains(body, want) {
			t.Errorf("front page lacks %q", want)
		}
	}
	for _, gone := range []string{"api/hello", "api/data", "testHello", "<button", "<a ", "Quickwit", "What kpr does",
		"/health", "status-text", "app.js", "ball"} {
		if strings.Contains(body, gone) {
			t.Errorf("front page still carries %q (mock, link, ball, or dropped section)", gone)
		}
	}
}

// The console's request metric counts real app-server traffic now that
// the mock endpoints are gone: every live request moves the counter.
// If this fails, the metric is decoration again.
func TestRequestCounterCountsLiveTraffic(t *testing.T) {
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Start(ctx) }()
	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/")

	before := GetAPIRequestCount()
	for _, path := range []string{"/api/status", "/", "/no-such-asset"} {
		resp, err := testClient.Get(base + path) //nolint:gosec,noctx // test-only loopback
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		drainAndClose(t, resp)
	}
	if got := GetAPIRequestCount(); got != before+3 {
		t.Errorf("counter moved by %d, want 3 for 3 live requests", got-before)
	}
}
