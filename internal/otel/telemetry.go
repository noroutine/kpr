package otel

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

// Instruments for the request pipeline, created lazily so the
// middleware works without a full Init (tests, partial wiring).
// initMetrics is idempotent across repeated Init calls.
var (
	initMetricsOnce sync.Once
	initMetricsErr  error

	httpRequests metric.Int64Counter
	httpDuration metric.Float64Histogram
)

func initMetrics() error {
	initMetricsOnce.Do(func() {
		initMetricsErr = buildMetrics(otel.Meter("nrtn.dev/catalyst/kpr/internal/otel"))
	})
	return initMetricsErr
}

// buildMetrics creates the request-pipeline instruments on m. Split
// from initMetrics so the creation error paths are unit-testable
// without fighting the Once guard.
func buildMetrics(m metric.Meter) error {
	var err error
	httpRequests, err = m.Int64Counter("kpr.http.server.requests",
		metric.WithDescription("HTTP requests served."))
	if err != nil {
		return err
	}
	httpDuration, err = m.Float64Histogram("kpr.http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("HTTP request duration in seconds."))
	if err != nil {
		return err
	}
	// Synthetic load signal for the example dashboard: a slow
	// sawtooth that visibly moves on every scrape. Clearly
	// named example — not a real queue.
	_, err = m.Float64ObservableGauge("kpr.example.queue_depth",
		metric.WithDescription("Example synthetic gauge proving the metrics path moves."),
		metric.WithFloat64Callback(observeDemoQueue),
	)
	return err
}

// observeDemoQueue is the gauge callback, named so the observation
// is unit-testable without a collection cycle.
func observeDemoQueue(_ context.Context, o metric.Float64Observer) error {
	o.Observe(demoQueueDepth(time.Now()), metric.WithAttributes(
		attribute.String("queue", "demo"),
	))
	return nil
}

// demoQueueDepth is the synthetic gauge value: seconds since epoch
// folded into 0-99. Pure so the callback math is unit-testable.
func demoQueueDepth(now time.Time) float64 {
	return float64(now.Unix() % 100)
}

// statusRecorder captures the status code for logging and metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// RequestTelemetry emits one structured access log per request and
// records request count/duration instruments. Disabled, it passes the
// request through untouched. Enabled, it must run INSIDE HTTPMiddleware
// so the span context (trace/span IDs) is already present.
func RequestTelemetry(next http.Handler, enabled bool) http.Handler {
	if !enabled {
		return next
	}
	if err := initMetrics(); err != nil {
		// Instruments are best-effort: a broken meter must not break
		// serving. Logging still works.
		Logger().Warn("otel metrics unavailable", "err", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		attrs := []attribute.KeyValue{
			attribute.String("method", r.Method),
			attribute.String("path", r.URL.Path),
			attribute.Int("status", rec.status),
		}
		if httpRequests != nil {
			httpRequests.Add(r.Context(), 1, metric.WithAttributes(attrs...))
		}
		if httpDuration != nil {
			httpDuration.Record(r.Context(), dur.Seconds(), metric.WithAttributes(attrs...))
		}

		// trace/span IDs ride as structured fields, not message
		// text: stdout stays one readable line, Quickwit gets
		// filterable attributes plus the OTLP envelope IDs.
		logAttrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", dur.Milliseconds(),
		}
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			logAttrs = append(logAttrs,
				"trace_id", sc.TraceID().String(),
				"span_id", sc.SpanID().String(),
			)
		}
		Logger().InfoContext(r.Context(), "http request", logAttrs...)
	})
}

// fanoutHandler duplicates each record to every contained handler:
// human-readable stdout plus the OTLP bridge.
type fanoutHandler []slog.Handler

func (f fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	for _, h := range f {
		if herr := h.Handle(ctx, r); herr != nil && err == nil {
			err = herr
		}
	}
	return err
}

func (f fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanoutHandler, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanoutHandler) WithGroup(name string) slog.Handler {
	out := make(fanoutHandler, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// newAccessLogger sends records to OTLP (structured, for
// Quickwit) via the slog bridge. Stdout stays out of the fanout:
// the terminal belongs to the live UI, not the audit trail.
func newAccessLogger(lp *sdklog.LoggerProvider) *slog.Logger {
	bridge := otelslog.NewHandler("kpr", otelslog.WithLoggerProvider(lp))
	return slog.New(fanoutHandler{bridge})
}
