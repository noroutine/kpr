package otel

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// Tracing knobs must flow from the resolved Config into the OTEL setup —
// a test-scoped Config (not the real environment) decides. If this
// fails, `serve` traces under the wrong identity or talks to the wrong
// collector.
func TestLoadConfigReadsCurrent(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithOTELEnabled(true).
		WithOTLPEndpoint("https://collector:4318").
		WithOTELServiceName("kpr-test").
		WithOTELServiceVersion("v0").
		WithOTELEnvironment("test").
		Build()))

	cfg := LoadConfig()
	if !cfg.Enabled || cfg.Endpoint != "collector:4318" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.ServiceName != "kpr-test" || cfg.ServiceVersion != "v0" || cfg.Environment != "test" {
		t.Errorf("identity = %+v", cfg)
	}
}

// With tracing disabled, Init must stay completely inert: no exporter,
// no provider, and a shutdown func that succeeds trivially. If this
// fails, the default `serve` (tracing off) pays setup cost or errors at
// startup for a feature nobody asked for.
func TestInitDisabledIsNoop(t *testing.T) {
	shutdown, err := Init(Config{Enabled: false})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// With tracing enabled but no spans ever recorded, Init and Shutdown must
// complete without touching the network: exporter construction is lazy
// and an empty batch flush exports nothing. If this fails, enabling
// tracing without a reachable collector breaks startup instead of
// degrading to dropped spans at export time.
// AttachStdout composes onto whatever Init attached: over a
// single handler it fans out to two legs, over an existing fanout
// it appends flat — never a fanout in a fanout. If this fails,
// serve's stdout leg nests and every record logs twice (or the
// OTLP leg drops off the composition).
func TestAttachStdoutComposesFlat(t *testing.T) {
	prevLog := accessLogger
	t.Cleanup(func() { accessLogger = prevLog })

	accessLogger = slog.New(slog.DiscardHandler)
	AttachStdout()
	h, ok := accessLogger.Handler().(fanoutHandler)
	if !ok || len(h) != 2 {
		t.Fatalf("handler = %T (%v), want flat 2-leg fanout", accessLogger.Handler(), accessLogger.Handler())
	}
	AttachStdout()
	h, ok = accessLogger.Handler().(fanoutHandler)
	if !ok || len(h) != 3 {
		t.Fatalf("handler = %T (%v), want flat 3-leg fanout", accessLogger.Handler(), accessLogger.Handler())
	}
	for _, leg := range h {
		if _, nested := leg.(fanoutHandler); nested {
			t.Errorf("leg = %T, want no nested fanout", leg)
		}
	}
}

func TestInitEnabledShutsDownClean(t *testing.T) {
	t.Cleanup(config.SetCurrent(config.NewBuilder().
		WithOTELEnabled(true).
		WithOTLPEndpoint("127.0.0.1:1").
		WithShutdownTimeout(1000000000).
		Build()))

	prevLog := accessLogger
	t.Cleanup(func() { accessLogger = prevLog })

	shutdown, err := Init(LoadConfig())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// A disabled middleware must pass the request through untouched: same
// status, same body, same handler. If this fails, flipping tracing off
// changes what clients observe.
func TestMiddlewareDisabledPassthrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("leaf"))
	})
	h := HTTPMiddleware(next, "svc", false)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot || rr.Body.String() != "leaf" {
		t.Errorf("got %d %q", rr.Code, rr.Body.String())
	}
}

// An enabled middleware must still serve through the default (noop)
// provider without a configured SDK: wrapping must not require global
// state to exist first. If this fails, enabling tracing on one route
// breaks it until the whole tracing stack is initialized.
func TestMiddlewareEnabledServes(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("leaf"))
	})
	h := HTTPMiddleware(next, "svc", true)

	req := httptest.NewRequest(http.MethodPost, "/api/data", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || rr.Body.String() != "leaf" {
		t.Errorf("got %d %q", rr.Code, rr.Body.String())
	}
}
