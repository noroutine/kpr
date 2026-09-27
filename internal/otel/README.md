# OpenTelemetry Support

kpr includes optional OpenTelemetry (OTEL) support for distributed tracing.

## Configuration

OpenTelemetry is **disabled by default**. Enable it via environment variables:

### Required
- `OTEL_ENABLED=true` - Enable OpenTelemetry

### Optional
- `OTEL_SERVICE_NAME` - Service name (default: "kpr")
- `OTEL_EXPORTER_OTLP_ENDPOINT` - OTLP endpoint (default: "localhost:4318")
  - Can include protocol (http:// or https://) - will be stripped automatically
  - Examples: `localhost:4318`, `http://localhost:4318`, `tempo:4318`
- `OTEL_ENVIRONMENT` - Environment name (default: "development")
- `OTEL_SERVICE_VERSION` - Service version (default: "dev")

## Usage

### Basic Setup

```bash
export OTEL_ENABLED=true
export OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4318
kpr serve
```

### With Jaeger (via OTLP)

```bash
# Run Jaeger with OTLP support
docker run -d --name jaeger \
  -p 16686:16686 \
  -p 4318:4318 \
  jaegertracing/all-in-one:latest

# Enable OTEL
export OTEL_ENABLED=true
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_SERVICE_NAME=kpr
export OTEL_ENVIRONMENT=production

kpr serve
```

Visit Jaeger UI at http://localhost:16686

### With Grafana Tempo

```bash
export OTEL_ENABLED=true
export OTEL_EXPORTER_OTLP_ENDPOINT=http://tempo:4318
export OTEL_SERVICE_NAME=kpr
kpr serve
```

### With Cloud Providers

**Honeycomb:**
```bash
export OTEL_ENABLED=true
export OTEL_EXPORTER_OTLP_ENDPOINT=https://api.honeycomb.io
export OTEL_EXPORTER_OTLP_HEADERS="x-honeycomb-team=YOUR_API_KEY"
```

**Datadog:**
```bash
export OTEL_ENABLED=true
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
# Configure Datadog Agent with OTLP receiver
```

## What's Instrumented

When enabled, OTEL automatically traces:
- All HTTP requests (management console on 9300)
- All HTTP requests (application on 8080)
- Request duration, status codes, paths
- Distributed trace context propagation

## Implementation Details

- Uses OTLP/HTTP exporter
- Insecure mode by default (configure TLS for production)
- Always samples (configure sampling in production)
- Graceful shutdown with 5s timeout
- Zero overhead when disabled

## Disable in Production

Simply don't set `OTEL_ENABLED=true`, or explicitly:

```bash
export OTEL_ENABLED=false
kpr serve
```

No OTEL dependencies are initialized when disabled.
