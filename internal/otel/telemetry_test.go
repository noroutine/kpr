package otel

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
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
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(trace.AlwaysSample()))
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

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(trace.AlwaysSample()))
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
