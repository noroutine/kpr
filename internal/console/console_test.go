package console

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
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
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
		WithReceiverHost("127.0.0.1").
		WithReceiverPort(19000).
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
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want HTML", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{"<title>Management console</title>", "/_app/"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard shell omits %q", want)
		}
	}
	if strings.Contains(body, "<style>") {
		t.Error("dashboard shell carries an inline style block; styling lives in the bundle")
	}
	// A clean render logs nothing: a success that logs an error
	// means the error branch fires when it shouldn't.
	if out := logs.String(); strings.Contains(out, "Error") {
		t.Errorf("clean render logged errors: %q", out)
	}
}

// The overview endpoint voices the launchpad: configured UI base
// URLs render as links, an unconfigured backend stays out. If
// this fails, the console links backends nobody runs — or hides
// the ones that do.
func TestOverviewObsLinks(t *testing.T) {
	testConfig(t)
	get := func() overviewData {
		req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
		rr := httptest.NewRecorder()
		(&Server{}).overviewHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var out overviewData
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	if got := get(); got.Obs.Standing != keeper.StandingUnavailable || len(got.Obs.Links) != 0 {
		t.Errorf("unconfigured launchpad = %+v, want unavailable with no links", got.Obs)
	}

	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithQuickwitURL("http://localhost:7280").
		WithJaegerURL("http://localhost:16686").
		Build()))
	got := get()
	if got.Obs.Standing != keeper.StandingOK || len(got.Obs.Links) != 2 {
		t.Fatalf("configured launchpad = %+v, want ok with two links", got.Obs)
	}
	for _, want := range []string{"http://localhost:7280", "http://localhost:16686"} {
		found := false
		for _, l := range got.Obs.Links {
			found = found || l.URL == want
		}
		if !found {
			t.Errorf("launchpad lacks %q: %+v", want, got.Obs.Links)
		}
	}
}

// A configured dir names the snapshot source on the model:
// dir or unconfigured, the page voices where the bundles
// came from. If this fails, the overview hides a configured
// source.
func TestOverviewNamesConfiguredSource(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().WithConfigDir("/etc/kpr/resources").Build()))
	out := getOverview(t, &Server{})
	if out.Source != "KPR_CONFIG_DIR=/etc/kpr/resources" {
		t.Errorf("source = %q, want the configured dir", out.Source)
	}
}

// A client that goes away mid-encode must not 500 the model:
// the encode error is logged and the handler returns. If this
// fails, one gone client wedges the overview goroutine.
func TestOverviewHandlerLogsEncodeFailure(t *testing.T) {
	testConfig(t)
	logs := captureLog(t)
	(&Server{}).overviewHandler(failWriter{}, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if !strings.Contains(logs.String(), "Error encoding overview JSON") {
		t.Errorf("log = %q, want the encode refusal logged", logs.String())
	}
}

// The runtime rides the overview, not the metrics: the toolchain
// version and platform name the build behind the page for
// operators, while scrapers get no go_version key. If this fails,
// the runtime rows regressed to the metrics grid or dropped the
// platform.
func TestOverviewVoicesRuntime(t *testing.T) {
	testConfig(t)
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	rr := httptest.NewRecorder()
	(&Server{}).overviewHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out overviewData
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rt := config.CurrentRuntime()
	if out.GoVersion != rt.GoVersion || out.GoOSArch != rt.GOOS+"/"+rt.GOARCH {
		t.Errorf("runtime = %q/%q, want %q/%q", out.GoVersion, out.GoOSArch, rt.GoVersion, rt.GOOS+"/"+rt.GOARCH)
	}
}

// The embedded bundle carries the shell, its hashed assets, and
// the icon: the Go side serves whatever `just web-build` copied
// in, file for file. If this fails, the embed and the build
// output drifted apart.
func TestStaticAssetsServe(t *testing.T) {
	fsys := staticFS()
	var sawHTML, sawJS bool
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("read embed root: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "index.html" {
			sawHTML = true
		}
		if e.IsDir() && e.Name() == "_app" {
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
		}
	}
	if !sawHTML {
		t.Error("embedded bundle has no index.html")
	}
	if !sawJS {
		t.Error("embedded bundle has no hashed JS under _app/")
	}
	if _, err := fsys.Open("favicon.svg"); err != nil {
		t.Errorf("embedded bundle has no favicon.svg: %v", err)
	}
}

// Browsers request /favicon.ico unprompted on every visit: it must
// answer the icon as SVG with a day of caching, refuse writes, and
// be linked from the dashboard head. If this fails, every page view
// logs a 404 nobody looks at until the logs do.
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

// Subpath prefixes do not apply to the SPA: the shell and the API
// serve at root, and the Svelte pages fetch root-absolute paths. A
// based console still serves the shell — the prefix only ever
// mattered to server-rendered hrefs, which are gone. If a base
// path ever prefixes the UI again, the pages and web/README must
// change together — never the Go side alone.
func TestIndexIgnoresBasePath(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithConsoleBasePath("/kpr").
		Build()))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	IndexHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "/_app/") {
		t.Error("based console lost the root-absolute asset refs the SPA needs")
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
	// Bytes first: decoding consumes the recorder's buffer.
	payload := rr.Body.Bytes()
	var body metricsData
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version != config.Version || body.Commit != config.Commit {
		t.Errorf("identity = %q/%q", body.Version, body.Commit)
	}
	// The toolchain version is dashboard furniture, not a metric:
	// scrapers get no go_version key. If this fails, /metrics
	// leaks build trivia scrapers never asked for.
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if _, ok := raw["go_version"]; ok {
		t.Errorf("metrics carries go_version = %v, want the key gone", raw["go_version"])
	}
	if body.Environment["KPR_APP_PORT"] != "18080" || body.Environment["KPR_RECEIVER_PORT"] != "19000" || body.Environment["KPR_REDIS_ADDR"] != "redis:6379" {
		t.Errorf("environment = %v", body.Environment)
	}
	if body.Environment["KPR_CONFIG_DIR"] != "" {
		t.Errorf("config dir = %q, want empty by default", body.Environment["KPR_CONFIG_DIR"])
	}
	if body.Environment["KPR_TIME_METHOD"] == "" || body.Environment["KPR_TIME_SERVER"] == "" {
		t.Errorf("clock source missing from metrics: %v", body.Environment)
	}
}

// A clock running before the binary started reads zero
// uptime, never negative: the start stamp pins the process,
// so a scripted Now in the past clamps instead of wrapping.
// If this fails, uptime underflows on skewed test clocks.
func TestMetricsHandlerClampsNegativeUptime(t *testing.T) {
	testConfig(t)
	before := startTime.Add(-time.Minute)
	s := &Server{Now: func() time.Time { return before }}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	s.metricsHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body metricsData
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.UptimeSeconds != 0 {
		t.Errorf("uptime = %d, want clamped 0", body.UptimeSeconds)
	}
}

// The UI base URLs ride the metrics environment so the SPA
// launchpad renders them: set shows, empty hides. If this
// fails, the console links backends nobody runs — or hides
// the ones that do.
func TestMetricsHandlerVoicesUILinks(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithQuickwitURL("http://localhost:7280").
		WithGrafanaURL("http://localhost:3000").
		Build()))
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
	for k, want := range map[string]string{
		"KPR_QUICKWIT_URL":   "http://localhost:7280",
		"KPR_GRAFANA_URL":    "http://localhost:3000",
		"KPR_JAEGER_URL":     "",
		"KPR_PROMETHEUS_URL": "",
	} {
		if body.Environment[k] != want {
			t.Errorf("environment[%s] = %q, want %q", k, body.Environment[k], want)
		}
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
		"/":             http.StatusOK,
		"/metrics":      http.StatusOK,
		"/health":       http.StatusOK,
		"/api/overview": http.StatusOK,
		"/favicon.ico":  http.StatusOK,
		// The file server answers what the bundle carries and
		// 404s the rest — no catch-all render, no silent miss.
		"/_app/definitely-not-here": http.StatusNotFound,
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
	testConfig(t)
	ln := loopbackListener(t)
	s := &Server{Listener: ln}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := captureLog(t)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	waitFor(t, "http://"+ln.Addr().String()+"/health")

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
	waitShutdown(t, s)
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

// A bare IPv6 bind must render bracketed: "::" prints as "[::]:9300",
// never ":::9300". If this fails, the console misreports where it
// listens on dual-stack defaults.
// A bare IPv6 bind must render bracketed: "::" prints as "[::]:9300",
// never ":::9300". If this fails, the console misreports where it
// listens on dual-stack defaults.
func TestOverviewBracketsIPv6Bind(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithManagementHost("::").
		WithManagementPort(19300).
		WithAppHost("::").
		WithAppPort(18080).
		WithReceiverHost("::").
		WithReceiverPort(19000).
		Build()))

	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	rr := httptest.NewRecorder()
	(&Server{}).overviewHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out overviewData
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, tc := range []struct{ got, want string }{
		{out.ManagementAddr, "[::]:19300"},
		{out.AppAddr, "[::]:18080"},
		{out.ReceiverAddr, "[::]:19000"},
	} {
		if tc.got != tc.want {
			t.Errorf("bind = %q, want %q", tc.got, tc.want)
		}
	}
}

// getOverview decodes one /api/overview response for the model
// tests below: a zero Server (like today's tests build) must
// still answer 200 with degraded cards, never 500.
func getOverview(t *testing.T, s *Server) overviewData {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	rr := httptest.NewRecorder()
	s.overviewHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out overviewData
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// bracketHost exists so the console never prints ":::9300": bare IPv6
// gets brackets, everything else passes through untouched.
// stubSentinelAPI serves one canned generation (or an error) for the
// proof card: manifest with a config digest, blob with the payload.
type stubSentinelAPI struct {
	man, blob []byte
	err       error
}

func (s stubSentinelAPI) GetManifest(context.Context, string, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.man, nil
}

func (s stubSentinelAPI) GetBlob(context.Context, string, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.blob, nil
}

// The overview must name the wired backend, where it lives,
// and the live sentinel generation — a file stack reads "file"
// with its dir, never Redis's name. If this fails, the console
// misreports the backend the operator must fix.
func TestOverviewShowsFileStoreAndLiveSentinel(t *testing.T) {
	testConfig(t)
	dir := t.TempDir()
	api := stubSentinelAPI{
		man:  []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc"}}`),
		blob: []byte(`{"v":1,"gen":"019-test-gen","ts":"2026-09-30T00:00:00Z","writer":"test"}`),
	}
	out := getOverview(t, &Server{Store: store.NewFileStore(dir), Sentinel: api})
	if out.Store.Name != "file" || !strings.Contains(out.Store.Detail, dir) {
		t.Errorf("store = %+v, want the file backend with its dir", out.Store)
	}
	if out.Sentinel.Standing != keeper.StandingOK || out.Sentinel.Gen != "019-test-gen" {
		t.Errorf("sentinel = %+v, want the live generation", out.Sentinel)
	}
	if out.Store.LockNote != "" && out.Store.LockNote != "locked" {
		t.Errorf("lock note = %q on a fresh file stack", out.Store.LockNote)
	}
}

// An erroring sentinel renders unproven through the whole model:
// the card reads the shared proof, so a registry error voices
// "unproven", never stale and never 500. If this fails, the
// overview borrows yesterday's generation on errors.
func TestOverviewRendersUnprovenOnSentinelError(t *testing.T) {
	testConfig(t)
	out := getOverview(t, &Server{Sentinel: stubSentinelAPI{err: errors.New("registry: 500")}})
	if out.Sentinel.Standing != keeper.StandingUnproven {
		t.Errorf("sentinel = %+v, want unproven on registry error", out.Sentinel)
	}
	if out.Sentinel.Gen != "" {
		t.Errorf("sentinel carries gen %q with no proof", out.Sentinel.Gen)
	}
}

// With no store and no sentinel API the backend cards degrade to
// named absences — "unavailable", "unproven" — and still answer
// 200. If this fails, a backend outage takes the whole console
// down with it.
func TestOverviewDegradesWithoutStoreOrSentinel(t *testing.T) {
	testConfig(t)
	out := getOverview(t, &Server{})
	if out.Store.Standing != keeper.StandingUnavailable {
		t.Errorf("store = %+v, want unavailable with no store wired", out.Store)
	}
	if out.Sentinel.Standing != keeper.StandingClosed {
		t.Errorf("sentinel = %+v, want closed with no sentinel wired", out.Sentinel)
	}
	if out.Store.LockNote == "locked" {
		t.Error("overview claims locked with no store wired")
	}
}

// The list fields always render as arrays, never null: the SPA
// reads `.length` off plan, activity, and bundles, and JSON
// renders a nil Go slice as null — a blank crashed page. If
// this fails, an empty backend crashes the dashboard.
func TestOverviewListsNeverNull(t *testing.T) {
	testConfig(t)
	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	rr := httptest.NewRecorder()
	(&Server{}).overviewHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{`"plan":[]`, `"activity":[]`, `"bundles":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview omits %q (empty list must render [], never null)", want)
		}
	}
}

// errUnlockStore fails the intent read: the card must voice "lock
// unknown", never collapse the error into unlocked (which would
// claim intent nobody recorded) or into locked (which would blame
// the operator for a backend failure). If this fails, a redis
// hiccup misreports write intent either way.
type errUnlockStore struct {
	store.Store
}

func (errUnlockStore) IsUnlocked(context.Context) (bool, error) {
	return false, errors.New("marker unreadable")
}

func TestOverviewShowsUnknownLockOnMarkerError(t *testing.T) {
	testConfig(t)
	out := getOverview(t, &Server{Store: errUnlockStore{store.NewMemStore()}})
	if out.Store.LockNote != "lock unknown" {
		t.Errorf("lock note = %q, want the unreadable marker voiced", out.Store.LockNote)
	}
	if out.Store.Standing != keeper.StandingDegraded {
		t.Errorf("standing = %q, want degraded on marker error", out.Store.Standing)
	}
}

func TestBracketHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"::", "[::]"},
		{"::1", "[::1]"},
		{"[::1]", "[::1]"},
		{"127.0.0.1", "127.0.0.1"},
		{"localhost", "localhost"},
		{"kpr", "kpr"},
	} {
		if got := bracketHost(tc.in); got != tc.want {
			t.Errorf("bracketHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A client that goes away mid-write must not 500 the shell: the
// write error is logged and the handler returns. If this fails,
// one gone client wedges the serving goroutine.
func TestIndexHandlerLogsWriteFailure(t *testing.T) {
	logs := captureLog(t)
	indexHandler(failWriter{}, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(logs.String(), "Error writing dashboard") {
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
// this fails, a broken build serves blank dashboards.
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
