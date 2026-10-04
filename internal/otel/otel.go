package otel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"

	"nrtn.dev/catalyst/kpr/internal/config"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Config holds OpenTelemetry configuration
type Config struct {
	Enabled        bool
	ServiceName    string
	ServiceVersion string
	Endpoint       string
	Environment    string
}

// LoadConfig loads OTEL configuration from the active config.Config —
// every OTEL_* variable is resolved there (see internal/config), so this
// package never reads the environment itself.
func LoadConfig() Config {
	cfg := config.Current()
	return Config{
		Enabled:        cfg.OTELEnabled,
		ServiceName:    cfg.OTELServiceName,
		ServiceVersion: cfg.OTELServiceVersion,
		Endpoint:       cfg.OTLPEndpoint,
		Environment:    cfg.OTELEnvironment,
	}
}

// accessLogger is the process-wide request logger. Stdout stays
// quiet: before Init records go nowhere, Init (when enabled)
// sends them to OTLP only. The terminal belongs to the live UI
// (a fourteen-thousand-row sweep must not scroll it); audit
// lives in the store ring. Per-channel routing (levels, opt-in
// stdout) is future work, tracked in docs/OBSERVABILITY.md.
var accessLogger = slog.New(slog.DiscardHandler)

// Logger returns the process-wide access logger used by
// RequestTelemetry. Never nil.
func Logger() *slog.Logger { return accessLogger }

// AttachStdout adds the human-readable text leg to the access
// logger: long-running services (serve) keep docker-logs-visible
// records while CLI one-shots stay hushed. Composes with
// whatever Init attached — call once, at service startup,
// after Init.
func AttachStdout() {
	text := slog.NewTextHandler(os.Stdout, nil)
	if h, ok := accessLogger.Handler().(fanoutHandler); ok {
		accessLogger = slog.New(append(h, text))
		return
	}
	accessLogger = slog.New(fanoutHandler{accessLogger.Handler(), text})
}

// Init initializes OpenTelemetry if enabled
func Init(cfg Config) (func(context.Context) error, error) {
	if !cfg.Enabled {
		log.Println("OpenTelemetry is disabled")
		return func(context.Context) error { return nil }, nil
	}

	log.Printf("Initializing OpenTelemetry: endpoint=%s service=%s env=%s",
		cfg.Endpoint, cfg.ServiceName, cfg.Environment)

	// Create resource with service information
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			"", // No schema URL to avoid version conflicts
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.DeploymentEnvironment(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// OTLP/gRPC exporters: the native tongue of collectors and of
	// Quickwit (otlp-traces / otlp-logs on its gRPC port).
	ctx := context.Background()
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(), // Use TLS in production
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create exporter: %w", err)
	}

	// Create trace provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// Set global tracer provider
	otel.SetTracerProvider(tp)

	// Set global propagator
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// OTLP/gRPC logs share the trace endpoint. Construction is lazy
	// — an unreachable backend fails at export time, not here.
	logExporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithInsecure(), // Use TLS in production
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)

	// Metrics ride the Prometheus exporter on the default registry;
	// the management console exposes it for scraping.
	promExporter, err := otelprom.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(promExporter),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// From here on the access log fans out to stdout and OTLP.
	accessLogger = newAccessLogger(lp)

	log.Println("OpenTelemetry initialized successfully")

	// Return shutdown function
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, config.Current().ShutdownTimeout)
		defer cancel()
		return errors.Join(
			tp.Shutdown(ctx),
			lp.Shutdown(ctx),
			mp.Shutdown(ctx),
		)
	}, nil
}
