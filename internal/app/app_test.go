package app

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
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

// /api/status is the machine contract for expected state: ok when
// the store answers, degraded when it doesn't, disabled with no
// store at all — always 200 while serving (NOK is unreachable).
// If this fails, monitors can't tell a blind store from a dead
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
func TestStaticFSHasIndex(t *testing.T) {
	fsys, err := staticFS()
	if err != nil {
		t.Fatalf("staticFS: %v", err)
	}
	f, err := fsys.Open("index.html")
	if err != nil {
		t.Fatalf("open index.html: %v", err)
	}
	_ = f.Close()
}

// The embedded bundle carries the shell, its hashed assets, and the
// icon: the Go side serves whatever `just web-build` copied in,
// file for file. If this fails, the embed and the build output
// drifted apart.
func TestStaticAssetsServe(t *testing.T) {
	fsys, err := staticFS()
	if err != nil {
		t.Fatalf("staticFS: %v", err)
	}
	var sawJS bool
	if err := fs.WalkDir(fsys, "_app", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".js") {
			sawJS = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk _app: %v", err)
	}
	if !sawJS {
		t.Error("embedded bundle has no hashed JS under _app/")
	}
	if _, err := fsys.Open("favicon.svg"); err != nil {
		t.Errorf("embedded bundle has no favicon.svg: %v", err)
	}
}

// Browsers request /favicon.ico unprompted on every visit: it must
// answer the icon as SVG with a day of caching and refuse writes.
// If this fails, every page view logs a 404 nobody looks at until
// the logs do.
func TestFaviconServesIcon(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rr := httptest.NewRecorder()
	faviconHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want SVG", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=86400") {
		t.Errorf("Cache-Control = %q, want a day of caching", cc)
	}

	post := httptest.NewRequest(http.MethodPost, "/favicon.ico", nil)
	postRR := httptest.NewRecorder()
	faviconHandler(postRR, post)
	if postRR.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", postRR.Code)
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

// A started server must serve the SPA shell at /, its hashed assets
// under /_app/, the keeper status at /api/status — and 404
// anything else — then shut down cleanly on context cancel. If
// this fails, `serve` comes up but doesn't actually serve, or it
// hangs on shutdown instead of draining.
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
		"/favicon.ico":   http.StatusOK,
		"/no-such-asset": http.StatusNotFound,
		// The file server answers what the bundle carries and
		// 404s the rest — no catch-all render, no silent miss.
		"/_app/definitely-not-here": http.StatusNotFound,
		// /events moved to the receiver's own port: the app
		// server no longer answers it.
		"/events": http.StatusNotFound,
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

// The startup line must name the socket it actually serves —
// "localhost" is a lie under any other bind (dual-stack `::`,
// a container hostname, an injected listener). If this fails,
// the operator reads the wrong address off the boot log.
func TestServerStartLogsBoundAddress(t *testing.T) {
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := captureLog(t)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	base := "http://" + ln.Addr().String()
	waitFor(t, base+"/")

	if want := "http://" + ln.Addr().String(); !strings.Contains(logs.String(), want) {
		t.Errorf("boot log lacks %q:\n%s", want, logs.String())
	}
	if out := logs.String(); strings.Contains(out, "localhost") {
		t.Errorf("boot log names localhost instead of the bound address:\n%s", out)
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

// The front page serves the built SPA shell: the kpr title, the
// hashed asset refs, and no server-rendered copy — the page
// fetches /api/status at runtime. If this fails, the landing
// page regressed to server rendering.
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
	for _, want := range []string{"kpr", "/_app/"} {
		if !strings.Contains(body, want) {
			t.Errorf("front page lacks %q", want)
		}
	}
	// The pitch survives the redesign: hero, commands, tagline.
	for _, want := range []string{"Push. Pull.", "docker push", "reap plans, sweep deletes"} {
		if !strings.Contains(body, want) {
			t.Errorf("front page lost %q (old landing copy)", want)
		}
	}
	if strings.Contains(body, "<style>") {
		t.Error("front page carries an inline style block; styling lives in the bundle")
	}
}

// A client that goes away mid-write must not 500 the shell: the
// write error is logged and the handler returns. If this fails,
// one gone client wedges the serving goroutine.
func TestIndexHandlerLogsWriteFailure(t *testing.T) {
	logs := captureLog(t)
	indexHandler(failWriter{}, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(logs.String(), "Error writing front page") {
		t.Errorf("log = %q, want the write refusal logged", logs.String())
	}
}

// failWriter refuses every write: the gone-mid-write client.
type failWriter struct{}

func (failWriter) Header() http.Header { return http.Header{} }
func (failWriter) Write([]byte) (int, error) {
	return 0, errors.New("client gone")
}
func (failWriter) WriteHeader(int) {}

// A broken embed fails loud, not silent: if the compiled-in
// bundle ever loses its files, the handlers 500 instead of
// serving empty pages. Staged by swapping the embed (the real
// one always carries its files), restored before return. If
// this fails, a broken build serves a blank app.
func TestIndexHandlerRefusesBrokenEmbed(t *testing.T) {
	prev := staticFiles
	staticFiles = embed.FS{}
	defer func() { staticFiles = prev }()

	rr := httptest.NewRecorder()
	indexHandler(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("GET / on a broken embed = %d, want 500", rr.Code)
	}
	rr = httptest.NewRecorder()
	faviconHandler(rr, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("GET /favicon.ico on a broken embed = %d, want 500", rr.Code)
	}
}

// Subpath prefixes do not apply to the SPA: the shell and the API
// serve at root. A based front page still serves the shell —
// the prefix only ever mattered to server-rendered hrefs, which
// are gone.
func TestIndexPageIgnoresBasePath(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithAppBasePath("/app").
		Build()))

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
	if body := string(raw); !strings.Contains(body, "/_app/") {
		t.Error("based front page lost the root-absolute asset refs the SPA needs")
	}

	// /events is not this server's route: it lives on the
	// receiver's own port (see internal/receiver). A 200 here would
	// mean the unbundling regressed.
	resp2, err := testClient.Post(base+"/events", "application/json", strings.NewReader(`{}`)) //nolint:gosec,noctx // test-only loopback
	if err != nil {
		t.Fatalf("POST /events: %v", err)
	}
	drainAndClose(t, resp2)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("POST /events = %d, want 404 (receiver owns this route now)", resp2.StatusCode)
	}
}

// The console's request metric counts real app-server traffic:
// every live request moves the counter, including the shell and
// its 404s. If this fails, the metric is decoration again.
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
