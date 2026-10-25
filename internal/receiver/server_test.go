package receiver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// testClient is a keep-alive-free HTTP client for server tests: every
// request gets a fresh connection that is closed after the response,
// so no pooled idle connection is outstanding when the test cancels
// the server.
var testClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// drainAndClose fully reads and closes a test response body.
func drainAndClose(t *testing.T, resp *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
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

// waitForEvents polls POST /events until it answers 202 or the deadline
// passes: the receiver owns no GET route, so readiness is the receiver
// contract itself (an empty envelope decodes fine and accepts).
func waitForEvents(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := testClient.Post(base+"/events", "application/json", strings.NewReader(`{}`)) //nolint:gosec,noctx // test-only, loopback, bounded by deadline
		if err == nil {
			drainAndClose(t, resp)
			if resp.StatusCode == http.StatusAccepted {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s/events", base)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The receiver owns /events and nothing else: a push records a row,
// garbage 400s, anything off-route 404s — then it stops cleanly on
// context cancel. If this fails, `serve` comes up but the registry
// coupling is not actually listening, or it hangs on shutdown.
func TestServerStartServesEventsAndStopsGracefully(t *testing.T) {
	ln := loopbackListener(t)
	s := &Server{Listener: ln, Store: store.NewMemStore()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()

	base := "http://" + ln.Addr().String()
	waitForEvents(t, base)

	logs := captureLog(t)
	resp, err := testClient.Post(base+"/events", "application/json", strings.NewReader(pushEnvelope())) //nolint:gosec,noctx // test-only loopback
	if err != nil {
		t.Fatalf("POST /events: %v", err)
	}
	drainAndClose(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("POST /events = %d, want 202", resp.StatusCode)
	}
	if rows, _ := s.Store.All(context.Background()); len(rows) != 1 {
		t.Errorf("rows = %d, want the pushed row recorded", len(rows))
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/api/status", http.StatusNotFound},
		{http.MethodPost, "/events-garbage", http.StatusNotFound},
	} {
		req, err := http.NewRequest(tc.method, base+tc.path, nil) //nolint:noctx // test-only loopback
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp, err := testClient.Do(req) //nolint:gosec // test-only loopback
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		drainAndClose(t, resp)
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
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
	select {
	case <-s.shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown watcher did not finish")
	}
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
	s := &Server{Listener: ln, Store: store.NewMemStore()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := captureLog(t)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	waitForEvents(t, "http://"+ln.Addr().String())

	if want := "http://" + ln.Addr().String() + "/events"; !strings.Contains(logs.String(), want) {
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

// A listener that dies underneath the server must surface as a Start
// error so the caller can react, rather than being swallowed. If this
// fails, `serve` believes a dead socket is still serving.
func TestServerStartServeError(t *testing.T) {
	ln := loopbackListener(t)
	_ = ln.Close() // die before Serve reaches it
	s := &Server{Listener: ln, Store: store.NewMemStore()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err == nil {
		t.Error("Start on a closed listener succeeded, want an error")
	}
	// Run the watcher now and drain it before returning: its writes
	// must not land in a later test's capture window.
	cancel()
	select {
	case <-s.shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown watcher did not finish")
	}
}

// A shutdown that cannot drain in time must say so loudly: a
// connection stuck mid-body never idles, so Shutdown outlives its
// deadline against it and the watcher logs instead of hanging or
// succeeding silent. If this fails, a wedged client turns `serve`
// shutdown into a silent hang past the deadline.
func TestServerStartShutdownTimeoutIsLogged(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithShutdownTimeout(time.Nanosecond).
		Build()))
	ln := loopbackListener(t)
	s := &Server{Listener: ln, Store: store.NewMemStore()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := captureLog(t)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	base := "http://" + ln.Addr().String()
	waitForEvents(t, base)

	// Hold a connection open mid-body: headers complete, body
	// short, never closed — the server side stays reading (never
	// idle) while Shutdown's nanosecond deadline is already gone.
	conn, err := net.Dial("tcp", ln.Addr().String()) //nolint:noctx // test-only loopback
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "POST /events HTTP/1.1\r\nHost: x\r\nContent-Length: 64\r\n\r\nstuck"); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	// A second, complete envelope proves the listener dequeued the
	// stuck one first (FIFO backlog): by the time this 202s, the
	// server owns a conn that will never idle.
	resp, err := testClient.Post(base+"/events", "application/json", strings.NewReader(`{}`)) //nolint:gosec,noctx // test-only loopback
	if err != nil {
		t.Fatalf("POST /events: %v", err)
	}
	drainAndClose(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /events = %d, want 202", resp.StatusCode)
	}

	cancel()
	// shutdownDone closes after the watcher logs (deferred close
	// runs last): waiting on it — not on Start's return, which only
	// needs the listener closed and routinely wins the race — makes
	// the read below deterministic.
	select {
	case <-s.shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown watcher did not finish")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after cancel")
	}
	if out := logs.String(); !strings.Contains(out, "shutdown error") {
		t.Errorf("forced shutdown timeout logged nothing, want the loud give-up:\n%s", out)
	}
}

// An unlistenable address must fail Start fast with an error, not block
// forever or panic. If this fails, a typo'd bind address hangs `serve`
// instead of reporting the problem.
func TestServerStartListenFailure(t *testing.T) {
	s := &Server{Host: "127.0.0.1", Port: -1, Store: store.NewMemStore()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err == nil {
		t.Error("Start with an invalid port succeeded, want an error")
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
	s := &Server{Listener: ln, Store: store.NewMemStore()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx) }()
	waitForEvents(t, "http://"+ln.Addr().String())

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
