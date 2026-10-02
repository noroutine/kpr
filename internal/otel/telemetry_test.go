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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// failMeter is a metric.Meter that fails creation on the named
// constructor and records every attempt, so buildMetrics' error
// branches are reachable without a real broken provider.
type failMeter struct {
	metric.Meter
	failOn string
	calls  *[]string
}

func (m failMeter) Int64Counter(_ string, _ ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	*m.calls = append(*m.calls, "counter")
	if m.failOn == "counter" {
		var c metric.Int64Counter
		return c, errors.New("counter down")
	}
	var c metric.Int64Counter
	return c, nil
}

func (m failMeter) Float64Histogram(_ string, _ ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	*m.calls = append(*m.calls, "histogram")
	if m.failOn == "histogram" {
		var h metric.Float64Histogram
		return h, errors.New("histogram down")
	}
	var h metric.Float64Histogram
	return h, nil
}

func (m failMeter) Float64ObservableGauge(_ string, _ ...metric.Float64ObservableGaugeOption) (metric.Float64ObservableGauge, error) {
	*m.calls = append(*m.calls, "gauge")
	if m.failOn == "gauge" {
		var g metric.Float64ObservableGauge
		return g, errors.New("gauge down")
	}
	var g metric.Float64ObservableGauge
	return g, nil
}

func resetInstruments(t *testing.T) {
	t.Helper()
	prevReq, prevDur := httpRequests, httpDuration
	t.Cleanup(func() { httpRequests, httpDuration = prevReq, prevDur })
}

// A counter-creation failure must surface immediately, before the
// histogram is even attempted. If this fails, serving continues with
// a half-built instrument set and no error recorded.
func TestBuildMetricsPropagatesCounterError(t *testing.T) {
	resetInstruments(t)
	var calls []string
	if err := buildMetrics(failMeter{failOn: "counter", calls: &calls}); err == nil {
		t.Fatal("buildMetrics = nil, want counter error")
	}
	if len(calls) != 1 || calls[0] != "counter" {
		t.Errorf("calls = %v, want [counter]", calls)
	}
}

// A histogram-creation failure must surface even though the counter
// was already created. If this fails, the duration instrument is
// silently missing while its error is swallowed.
func TestBuildMetricsPropagatesHistogramError(t *testing.T) {
	resetInstruments(t)
	var calls []string
	if err := buildMetrics(failMeter{failOn: "histogram", calls: &calls}); err == nil {
		t.Fatal("buildMetrics = nil, want histogram error")
	}
	if len(calls) != 2 || calls[0] != "counter" || calls[1] != "histogram" {
		t.Errorf("calls = %v, want [counter histogram]", calls)
	}
}

// A gauge-creation failure must surface even though counter and
// histogram already exist. If this fails, the example gauge is
// silently missing while its error is swallowed.
func TestBuildMetricsPropagatesGaugeError(t *testing.T) {
	resetInstruments(t)
	var calls []string
	if err := buildMetrics(failMeter{failOn: "gauge", calls: &calls}); err == nil {
		t.Fatal("buildMetrics = nil, want gauge error")
	}
	if len(calls) != 3 || calls[2] != "gauge" {
		t.Errorf("calls = %v, want [counter histogram gauge]", calls)
	}
}

// The recorder unwraps to its writer: middleware stacking
// (ResponseController for flush/hijack) sees through it. If this
// fails, Flush and Hijack stop working behind telemetry.
func TestStatusRecorderUnwraps(t *testing.T) {
	inner := httptest.NewRecorder()
	r := &statusRecorder{ResponseWriter: inner}
	if r.Unwrap() != http.ResponseWriter(inner) {
		t.Error("Unwrap did not return the wrapped writer")
	}
}

// Enabled is any-enabled: all-quiet handlers vote false, one loud
// vote carries. If this fails, a fully-muted fanout still logs (or
// a live one goes silent).
func TestFanoutEnabledIsAnyEnabled(t *testing.T) {
	var a, b bytes.Buffer
	quiet := fanoutHandler{
		slog.NewTextHandler(&a, &slog.HandlerOptions{Level: slog.LevelError}),
		slog.NewJSONHandler(&b, &slog.HandlerOptions{Level: slog.LevelError}),
	}
	if quiet.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("all-muted fanout enabled debug, want false")
	}
	if !quiet.Enabled(context.Background(), slog.LevelError) {
		t.Error("all-muted fanout disabled error, want true")
	}
}

// Attrs and groups fan out to every handler: structured fields
// must not vanish on one leg. If this fails, one output loses
// fields the other keeps.
func TestFanoutAttrsAndGroupsFanOut(t *testing.T) {
	var a, b bytes.Buffer
	h := fanoutHandler{
		slog.NewTextHandler(&a, nil),
		slog.NewJSONHandler(&b, nil),
	}.WithAttrs([]slog.Attr{slog.String("k", "v")})
	slog.New(h).Info("hello")
	if !strings.Contains(a.String(), "k=v") || !strings.Contains(b.String(), `"k":"v"`) {
		t.Errorf("attrs lost a leg: text=%q json=%q", a.String(), b.String())
	}
	var c, d bytes.Buffer
	g := fanoutHandler{
		slog.NewTextHandler(&c, nil),
		slog.NewJSONHandler(&d, nil),
	}.WithGroup("grp")
	slog.New(g).Info("hello", "k", "v")
	if !strings.Contains(c.String(), "grp") || !strings.Contains(d.String(), "grp") {
		t.Errorf("group lost a leg: text=%q json=%q", c.String(), d.String())
	}
}

// A collection cycle observes the demo gauge: the callback fires
// through a real SDK reader, proving the gauge is wired to the
// meter (not just constructed). If this fails, the dashboard
// signal is registered but never moves.
func TestObserveDemoQueueTagsDemo(t *testing.T) {
	resetInstruments(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if err := buildMetrics(provider.Meter("test")); err != nil {
		t.Fatalf("buildMetrics: %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var sawGauge bool
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "kpr.example.queue_depth" {
				continue
			}
			sawGauge = true
			gauge, ok := m.Data.(metricdata.Gauge[float64])
			if !ok || len(gauge.DataPoints) != 1 {
				t.Fatalf("gauge data = %#v, want one float64 point", m.Data)
			}
			dp := gauge.DataPoints[0]
			if dp.Value < 0 || dp.Value >= 100 {
				t.Errorf("gauge value = %v, want the 0-99 sawtooth", dp.Value)
			}
			v, ok := dp.Attributes.Value(attribute.Key("queue"))
			if !ok || v.AsString() != "demo" {
				t.Errorf("gauge attrs = %v, want queue=demo", dp.Attributes)
			}
		}
	}
	if !sawGauge {
		t.Error("no kpr.example.queue_depth in the collection, want the demo gauge observed")
	}
}

// A healthy meter must build every instrument in order. If this
// fails, a later refactor dropped an instrument from the set while
// the error branches still pass.
func TestBuildMetricsSuccessBuildsAll(t *testing.T) {
	resetInstruments(t)
	var calls []string
	if err := buildMetrics(failMeter{failOn: "none", calls: &calls}); err != nil {
		t.Fatalf("buildMetrics = %v, want nil", err)
	}
	want := []string{"counter", "histogram", "gauge"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}
