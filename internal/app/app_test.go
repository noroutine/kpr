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

	"nrtn.dev/catalyst/kpr/internal/config"
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

// A client hitting the hello endpoint must get the greeting, the running
// binary's version, and a request counter that actually counts. If this
// fails, the console's request metrics silently disagree with reality.
func TestHelloHandler(t *testing.T) {
	before := GetAPIRequestCount()
	req := httptest.NewRequest(http.MethodGet, "/api/hello", nil)
	rr := httptest.NewRecorder()
	HelloHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body HelloResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Message != "Hello from kpr!" {
		t.Errorf("message = %q", body.Message)
	}
	if body.Version != config.Version {
		t.Errorf("version = %q, want running binary's %q", body.Version, config.Version)
	}
	if GetAPIRequestCount() != before+1 {
		t.Error("request counter did not increment")
	}
}

// A client hitting the data endpoint must get all five sample items with
// a matching count. If this fails, the example API contract the SPA
// consumes is broken.
func TestDataHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	rr := httptest.NewRecorder()
	DataHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body DataResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != len(body.Items) || len(body.Items) != 5 {
		t.Errorf("count = %d for %d items, want 5 and 5", body.Count, len(body.Items))
	}
}

// errWriter is a ResponseWriter whose Write always fails.
type errWriter struct{ header http.Header }

func (w errWriter) Header() http.Header       { return w.header }
func (w errWriter) Write([]byte) (int, error) { return 0, errors.New("nope") }
func (w errWriter) WriteHeader(int)           {}

// A broken connection mid-encode must not panic the handler: the error is
// logged and the handler returns. If this fails, one wedged client can
// take down the serving goroutine with it — or failures go silent while
// successes log, and nobody can tell which happened.
func TestHandlersTolerateEncodeError(t *testing.T) {
	logs := captureLog(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	HelloHandler(errWriter{header: http.Header{}}, req)
	DataHandler(errWriter{header: http.Header{}}, req)
	if n := strings.Count(logs.String(), "Error encoding JSON"); n != 2 {
		t.Errorf("logged %d encode errors, want 2 (one per handler)", n)
	}
}

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

// A started server must serve the SPA at /, the API under /api, and 404
// anything else — then shut down cleanly on context cancel. If this
// fails, `serve` comes up but doesn't actually serve, or it hangs on
// shutdown instead of draining.
func TestServerStartServesAndStopsGracefully(t *testing.T) {
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/api/hello")

	// Healthy serving logs nothing: a success that logs an error (or a
	// failure that stays silent) means the error branches lie. Handler
	// writes complete before the fully-read response, so one read
	// observes them all.
	logs := captureLog(t)
	for path, want := range map[string]int{
		"/":              http.StatusOK,
		"/api/hello":     http.StatusOK,
		"/api/data":      http.StatusOK,
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
	waitFor(t, "http://"+ln.Addr().String()+"/api/hello")

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
