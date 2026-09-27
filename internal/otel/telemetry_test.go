package otel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// A disabled telemetry middleware must pass the request through
// untouched: same status, same body, no log side effects required. If
// this fails, flipping tracing off changes what clients observe.
func TestRequestTelemetryDisabledPassthrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("leaf"))
	})
	h := RequestTelemetry(next, false)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot || rr.Body.String() != "leaf" {
		t.Errorf("got %d %q", rr.Code, rr.Body.String())
	}
}

// An enabled middleware must preserve the response and emit exactly
// one access log carrying the request fields plus the trace/span IDs
// from the span context — as structured fields, not message text. If
// this fails, Quickwit gets either no correlation IDs or an inflated
// message line.
func TestRequestTelemetryLogsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	prev := accessLogger
	accessLogger = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { accessLogger = prev })

	// A real SDK span so the context carries valid IDs (the global
	// noop provider would leave them invalid and unlogged).
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	h := RequestTelemetry(next, true)

	req := httptest.NewRequest(http.MethodGet, "/api/hello", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("got %d %q", rr.Code, rr.Body.String())
	}

	line := buf.String()
	for _, want := range []string{
		`msg="http request"`,
		`method=GET`,
		`path=/api/hello`,
		`status=200`,
		"trace_id=" + span.SpanContext().TraceID().String(),
		"span_id=" + span.SpanContext().SpanID().String(),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %q", want, line)
		}
	}
}

// Production nesting puts RequestTelemetry INSIDE HTTPMiddleware so
// the span is already in context. If this fails, a refactor flipped
// the nesting and every access log silently loses its trace IDs.
func TestTelemetryInsideMiddlewareSeesSpan(t *testing.T) {
	var buf bytes.Buffer
	prev := accessLogger
	accessLogger = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { accessLogger = prev })

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		_ = tp.Shutdown(context.Background())
	})

	leaf := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := HTTPMiddleware(RequestTelemetry(leaf, true), "svc", true)

	req := httptest.NewRequest(http.MethodGet, "/ordered", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if !strings.Contains(buf.String(), "trace_id=") {
		t.Errorf("telemetry inside middleware logged no trace_id: %q", buf.String())
	}
}

// Without a span in context there are no IDs to log — the middleware
// must still serve and log the request fields. If this fails,
// untraced requests (health probes, pre-middleware routes) error out
// or go unlogged.
func TestRequestTelemetryWithoutSpan(t *testing.T) {
	var buf bytes.Buffer
	prev := accessLogger
	accessLogger = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { accessLogger = prev })

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	h := RequestTelemetry(next, true)

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("got %d", rr.Code)
	}
	if !strings.Contains(buf.String(), "path=/health") {
		t.Errorf("missing request fields: %q", buf.String())
	}
	if strings.Contains(buf.String(), "trace_id=") {
		t.Errorf("unexpected trace_id without span: %q", buf.String())
	}
}

// The fanout handler must deliver every record to every contained
// handler: stdout keeps its line, OTLP gets its record. If this
// fails, one sink silently starves.
func TestFanoutDeliversToAll(t *testing.T) {
	var a, b bytes.Buffer
	h := fanoutHandler{
		slog.NewTextHandler(&a, nil),
		slog.NewJSONHandler(&b, nil),
	}
	l := slog.New(h)
	l.Info("hello", "k", "v")

	if !strings.Contains(a.String(), "hello") || !strings.Contains(b.String(), "hello") {
		t.Errorf("fanout dropped a record: text=%q json=%q", a.String(), b.String())
	}
}

// The process-wide logger must never be nil, even before Init.
func TestLoggerNeverNil(t *testing.T) {
	if Logger() == nil {
		t.Error("Logger() is nil")
	}
}

// The synthetic gauge folds epoch seconds into 0-99. If this fails,
// the dashboard sawtooth reports the wrong load shape.
func TestDemoQueueDepth(t *testing.T) {
	cases := map[int64]float64{0: 0, 1: 1, 150: 50, 199: 99, 200: 0}
	for unix, want := range cases {
		if got := demoQueueDepth(time.Unix(unix, 0)); got != want {
			t.Errorf("demoQueueDepth(%d) = %v, want %v", unix, got, want)
		}
	}
}

// With instruments missing (meter failed), requests must still serve
// and log: metrics are best-effort, never load-bearing. If this fails,
// a broken meter takes down serving instead of degrading silently.
func TestRequestTelemetryNilInstruments(t *testing.T) {
	var buf bytes.Buffer
	prevLog := accessLogger
	accessLogger = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { accessLogger = prevLog })

	prevReq, prevDur := httpRequests, httpDuration
	httpRequests, httpDuration = nil, nil
	t.Cleanup(func() { httpRequests, httpDuration = prevReq, prevDur })

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	h := RequestTelemetry(next, true)

	req := httptest.NewRequest(http.MethodGet, "/fragile", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("got %d %q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(buf.String(), "path=/fragile") {
		t.Errorf("request unlogged without instruments: %q", buf.String())
	}
}

// stubHandler is a slog.Handler with a scripted error for fanout tests.
type stubHandler struct {
	buf *bytes.Buffer
	err error
}

func (s *stubHandler) Enabled(context.Context, slog.Level) bool { return true }
func (s *stubHandler) Handle(_ context.Context, r slog.Record) error {
	s.buf.WriteString(r.Message)
	return s.err
}
func (s *stubHandler) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *stubHandler) WithGroup(string) slog.Handler      { return s }

// A failing sink must surface its error (first one wins) while the
// healthy sinks still get their record: silent fanout loss means
// Quickwit starves with stdout looking fine. If this fails, sink
// errors vanish.
func TestFanoutPropagatesFirstError(t *testing.T) {
	var okBuf bytes.Buffer
	ok := &stubHandler{buf: &okBuf}
	bad := &stubHandler{buf: &bytes.Buffer{}, err: errors.New("sink down")}
	later := &stubHandler{buf: &bytes.Buffer{}, err: errors.New("later")}

	h := fanoutHandler{ok, bad, later}
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
	err := h.Handle(context.Background(), rec)

	if err == nil || err.Error() != "sink down" {
		t.Errorf("got %v, want the first sink error", err)
	}
	if okBuf.String() != "hello" {
		t.Errorf("healthy sink starved: %q", okBuf.String())
	}
}

// The global provider swap in Init must not disturb the default
// meter used by initMetrics in tests.
func TestInitMetricsIdempotent(t *testing.T) {
	otel.SetMeterProvider(otel.GetMeterProvider())
	if err := initMetrics(); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := initMetrics(); err != nil {
		t.Fatalf("second: %v", err)
	}
}
