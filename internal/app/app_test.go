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

// pingFailStore is a Store whose Ping always fails: the degraded half of
// the health contract without needing a dead redis.
type pingFailStore struct{ store.Store }

func (pingFailStore) Ping(context.Context) error { return errors.New("redis down") }

// The status ball on the front page reads /health: it must report the
// process alive with the running binary's version, and say whether the
// receiver's redis is reachable — degraded, not dead, when redis is
// down, because boot degrades the same way. If this fails, the page
// either shows a green ball over a blind receiver or cries red on a
// healthy keeper.
func TestHealthHandler(t *testing.T) {
	for name, st := range map[string]struct {
		store         store.Store
		status, redis string
	}{
		"reachable": {store.NewMemStore(), "ok", "reachable"},
		"down":      {pingFailStore{}, "degraded", "unreachable"},
		"disabled":  {nil, "ok", "disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rr := httptest.NewRecorder()
			HealthHandler(st.store)(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			var body HealthResponse
			if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Status != st.status || body.Redis != st.redis {
				t.Errorf("got %s/%s, want %s/%s", body.Status, body.Redis, st.status, st.redis)
			}
			if body.Version != config.Version {
				t.Errorf("version = %q, want running binary's %q", body.Version, config.Version)
			}
		})
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
func TestHealthToleratesEncodeError(t *testing.T) {
	logs := captureLog(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	HealthHandler(store.NewMemStore())(errWriter{header: http.Header{}}, req)
	if n := strings.Count(logs.String(), "Error encoding JSON"); n != 1 {
		t.Errorf("logged %d encode errors, want 1", n)
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
	waitFor(t, base+"/health")

	// Healthy serving logs nothing: a success that logs an error (or a
	// failure that stays silent) means the error branches lie. Handler
	// writes complete before the fully-read response, so one read
	// observes them all.
	logs := captureLog(t)
	for path, want := range map[string]int{
		"/":              http.StatusOK,
		"/health":        http.StatusOK,
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
	waitFor(t, "http://"+ln.Addr().String()+"/health")

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
// forget — homelab wording, not ephemeral-only), the pipeline it
// actually runs, and a status ball fed by the real /health. No mock
// API buttons, no links section. If this fails, the template demo is
// back or the ball reads a dead endpoint.
func TestIndexPageIsKeeperFront(t *testing.T) {
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Start(ctx) }()
	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/health")

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
	for _, want := range []string{"Push. Pull.", "/health", "reap", "sweep", "keeper"} {
		if !strings.Contains(body, want) {
			t.Errorf("front page lacks %q", want)
		}
	}
	for _, gone := range []string{"api/hello", "api/data", "testHello", "<button", "<a ", "Quickwit", "What kpr does"} {
		if strings.Contains(body, gone) {
			t.Errorf("front page still carries %q (mock, link, or dropped section)", gone)
		}
	}
}

// The status ball is live, not paint: the page script re-reads /health
// on a timer so a dead keeper turns the ball red without a reload. If
// this fails, the ball is a one-shot snapshot again.
func TestFrontPagePollsHealth(t *testing.T) {
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Start(ctx) }()
	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/health")

	resp, err := testClient.Get(base + "/static/app.js") //nolint:gosec,noctx // test-only loopback
	if err != nil {
		t.Fatalf("GET /static/app.js: %v", err)
	}
	raw, err := io.ReadAll(resp.Body)
	drainAndClose(t, resp)
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(raw)
	for _, want := range []string{"/health", "setInterval"} {
		if !strings.Contains(js, want) {
			t.Errorf("page script lacks %q (no live ball)", want)
		}
	}
	if strings.Contains(js, "api/hello") || strings.Contains(js, "api/data") {
		t.Errorf("page script still calls the mock API")
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
	waitFor(t, base+"/health")

	before := GetAPIRequestCount()
	for _, path := range []string{"/health", "/health", "/"} {
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
