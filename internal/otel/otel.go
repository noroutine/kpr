package otel

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Config holds OpenTelemetry configuration
type Config struct {
	Enabled     bool
	ServiceName string
	Endpoint    string
	Environment string
}

// LoadConfig loads OTEL configuration from environment variables
func LoadConfig() Config {
	enabled := os.Getenv("OTEL_ENABLED") == "true"

	endpoint := getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4318")
	// Strip protocol if present (otlptracehttp.WithEndpoint expects host:port only)
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")

	return Config{
		Enabled:     enabled,
		ServiceName: getEnv("OTEL_SERVICE_NAME", "kpr"),
		Endpoint:    endpoint,
		Environment: getEnv("OTEL_ENVIRONMENT", "development"),
	}
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
			semconv.ServiceVersion(getEnv("OTEL_SERVICE_VERSION", "dev")),
			semconv.DeploymentEnvironment(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create OTLP HTTP exporter
	ctx := context.Background()
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.Endpoint),
		otlptracehttp.WithInsecure(), // Use TLS in production
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

	log.Println("OpenTelemetry initialized successfully")

	// Return shutdown function
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
